package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	MaxBodyBytes = 5 << 20

	// FetchTimeout is the firm ceiling on one outbound scrape, covering DNS,
	// connect, TLS, the response, and reading the body. It is applied as both a
	// request context deadline and http.Client.Timeout so that a target which
	// accepts the connection then stalls cannot hold the slot open: whichever
	// fires first aborts the request and the body read.
	FetchTimeout = 15 * time.Second
)

var allowedSchemes = map[string]bool{"http": true, "https": true}

// IsTimeout reports whether err came from a network operation that ran out of
// time, covering both context deadlines and transport-level timeouts.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 0 {
		return false
	}
	return true
}

// safeDialer refuses connections to non-public addresses at connect time, which
// also covers DNS rebinding between validation and the actual connection. The
// dialer timeout is deliberately short: the request context is the authority on
// the overall deadline, and a fast dial failure surfaces the error promptly.
//
// allowPrivateTargets exists for tests, which must reach a loopback httptest
// server. It is never enabled in the serving path.
func safeDialer(timeout time.Duration, allowPrivateTargets bool) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivateTargets {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if !IsPublicIP(ip) {
				return fmt.Errorf("blocked non-public address %s", host)
			}
			return nil
		},
	}
}

func ValidateTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("malformed url")
	}
	if !allowedSchemes[strings.ToLower(u.Scheme)] {
		return nil, errors.New("only http and https urls are allowed")
	}
	if u.Hostname() == "" {
		return nil, errors.New("url is missing a host")
	}
	if u.User != nil {
		return nil, errors.New("url must not contain credentials")
	}
	if host := u.Hostname(); net.ParseIP(host) != nil && !IsPublicIP(net.ParseIP(host)) {
		return nil, errors.New("url resolves to a non-public address")
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return nil, errors.New("could not resolve host")
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return nil, errors.New("url resolves to a non-public address")
		}
	}
	return u, nil
}

func NewFetchClient() *http.Client {
	return NewFetchClientFor(false)
}

func NewFetchClientFor(allowPrivateTargets bool) *http.Client {
	dialer := safeDialer(FetchTimeout, allowPrivateTargets)
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		// Idle connections are capped and reaped so a burst of one-off scrapes
		// cannot accumulate sockets that outlive their request.
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		DisableKeepAlives:     false,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		// Client.Timeout bounds the whole exchange including the body read, so a
		// target that sends headers and then trickles bytes is still cut off. The
		// caller's context deadline is the same duration and additionally covers
		// DNS resolution, which Client.Timeout does not always reach.
		Timeout: FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !allowedSchemes[strings.ToLower(req.URL.Scheme)] {
				return errors.New("redirect to a disallowed scheme")
			}
			ips, err := net.LookupIP(req.URL.Hostname())
			if err != nil {
				return errors.New("could not resolve redirect target")
			}
			for _, ip := range ips {
				if !IsPublicIP(ip) {
					return errors.New("blocked redirect to a non-public address")
				}
			}
			return nil
		},
	}
}

type Result struct {
	Body        []byte
	FinalURL    string
	StatusCode  int
	ContentType string
	Truncated   bool
	Duration    time.Duration
}

func Fetch(ctx context.Context, client *http.Client, u *url.URL) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", "ipRoyal-Scraper/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return Result{}, err
	}
	truncated := false
	if len(body) > MaxBodyBytes {
		body = body[:MaxBodyBytes]
		truncated = true
	}

	return Result{
		Body:        body,
		FinalURL:    resp.Request.URL.String(),
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Truncated:   truncated,
		Duration:    time.Since(started),
	}, nil
}
