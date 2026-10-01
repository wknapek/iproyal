package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ipRoyal/cmManager"
	"ipRoyal/scraper"

	"github.com/gin-gonic/gin"
)

// newTestHandler wires a handler with a fixed admin token and an empty store.
func newTestHandler(t *testing.T) (*Handler, *cmManager.CustomerManager, *gin.Engine) {
	t.Helper()
	t.Setenv("ADMIN_TOKEN", "adm")
	cm := cmManager.NewCustomerManager()
	h := NewHandler(cm)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r)
	return h, cm, r
}

// allowLoopbackTarget points the handler at a local server. The real guard rejects
// loopback twice over, in ValidateTarget and again in the dialer, so both are
// relaxed here: validation is replaced with the scheme-and-host checks only, and
// the client is built with private targets permitted. Production code paths are
// untouched.
func allowLoopbackTarget(t *testing.T) {
	t.Helper()
	origValidate, origClient := validateTarget, newFetchClient
	validateTarget = func(raw string) (*url.URL, error) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, errors.New("malformed url")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, errors.New("only http and https urls are allowed")
		}
		if u.Hostname() == "" {
			return nil, errors.New("url is missing a host")
		}
		return u, nil
	}
	newFetchClient = func() *http.Client { return scraper.NewFetchClientFor(true) }
	t.Cleanup(func() { validateTarget, newFetchClient = origValidate, origClient })
}

func newCustomer(t *testing.T, r *gin.Engine, name string) string {
	t.Helper()
	body := `{"name":"` + name + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp createUserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create user response: %v", err)
	}
	return resp.ID
}

func scrapeURL(t *testing.T, r *gin.Engine, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/scrape?"+query, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func remaining(t *testing.T, cm *cmManager.CustomerManager, token string) uint {
	t.Helper()
	got, err := cm.Remaining(token)
	if err != nil {
		t.Fatalf("remaining for %s: %v", token, err)
	}
	return got
}

func TestCreateUserRequiresAdminToken(t *testing.T) {
	_, _, r := newTestHandler(t)

	for _, header := range []string{"", "Bearer wrong", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(`{"name":"acme"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status %d, want %d", header, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestCreateUserValidatesName(t *testing.T) {
	_, _, r := newTestHandler(t)

	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 129) + `"}`, `not json`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer adm")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

// The request shape is checked before the token, so a caller gets a precise 400
// instead of being told their token is bad.
func TestScrapeRequiresUrlAndValidFormat(t *testing.T) {
	_, _, r := newTestHandler(t)
	token := newCustomer(t, r, "acme")

	rec := scrapeURL(t, r, "token="+token)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing url: status %d, want %d", rec.Code, http.StatusBadRequest)
	}

	rec = scrapeURL(t, r, "url=https://example.com&format=xml&token="+token)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad format: status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestScrapeRejectsBadToken(t *testing.T) {
	_, _, r := newTestHandler(t)

	rec := scrapeURL(t, r, "url=https://example.com&format=json")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	rec = scrapeURL(t, r, "url=https://example.com&token=garbage")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestScrapeBlocksPrivateTargets(t *testing.T) {
	_, cm, r := newTestHandler(t)
	token := newCustomer(t, r, "acme")

	for _, raw := range []string{
		"http://127.0.0.1/admin",
		"http://localhost/admin",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/",
		"http://user:pass@example.com/",
		"http:///",
	} {
		rec := scrapeURL(t, r, "url="+url.QueryEscape(raw)+"&format=json&token="+token)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("target %s: status %d, want %d", raw, rec.Code, http.StatusBadRequest)
		}
	}

	// A rejected target must not have touched the balance.
	if got := remaining(t, cm, token); got != cmManager.MaxRequests {
		t.Errorf("blocked targets consumed allowance: %d left, want %d", got, cmManager.MaxRequests)
	}
}

func TestValidationFailuresDoNotConsumeAllowance(t *testing.T) {
	_, cm, r := newTestHandler(t)
	token := newCustomer(t, r, "acme")

	for _, q := range []string{
		"",
		"url=https://example.com&format=xml&token=" + token,
		"url=http://127.0.0.1/&token=" + token,
	} {
		if rec := scrapeURL(t, r, q); rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status %d, want %d", q, rec.Code, http.StatusBadRequest)
		}
	}

	if got := remaining(t, cm, token); got != cmManager.MaxRequests {
		t.Errorf("invalid requests consumed allowance: %d left, want %d", got, cmManager.MaxRequests)
	}
}

func TestPerCustomerAllowanceEndToEnd(t *testing.T) {
	allowLoopbackTarget(t)
	_, cm, r := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	acme := newCustomer(t, r, "acme")
	other := newCustomer(t, r, "other")

	// One customer's spending must not touch another's.
	if rec := scrapeURL(t, r, "url="+url.QueryEscape(srv.URL)+"&format=html&token="+acme); rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := remaining(t, cm, acme); got != cmManager.MaxRequests-1 {
		t.Errorf("acme balance: %d, want %d", got, cmManager.MaxRequests-1)
	}
	if got := remaining(t, cm, other); got != cmManager.MaxRequests {
		t.Errorf("other balance changed: %d, want %d", got, cmManager.MaxRequests)
	}
}

func TestSuccessfulScrapeIsCharged(t *testing.T) {
	allowLoopbackTarget(t)
	_, cm, r := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	token := newCustomer(t, r, "acme")
	q := "url=" + url.QueryEscape(srv.URL) + "&format=json&token=" + token

	for i := uint(0); i < 3; i++ {
		if rec := scrapeURL(t, r, q); rec.Code != http.StatusOK {
			t.Fatalf("scrape %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
	}
	if got, want := remaining(t, cm, token), uint(cmManager.MaxRequests-3); got != want {
		t.Errorf("balance after 3 scrapes: %d, want %d", got, want)
	}

	// Draining the balance must produce 402, and stay there.
	for i := uint(0); i < cmManager.MaxRequests-3; i++ {
		if rec := scrapeURL(t, r, q); rec.Code != http.StatusOK {
			t.Fatalf("drain %d: status %d", i, rec.Code)
		}
	}
	rec := scrapeURL(t, r, q)
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("exhausted: status %d, want %d", rec.Code, http.StatusPaymentRequired)
	}
}

func TestFailedScrapeIsRefunded(t *testing.T) {
	allowLoopbackTarget(t)
	_, cm, r := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("partial"))
	}))
	target := srv.URL
	srv.Close() // nothing is listening now, so the fetch fails

	token := newCustomer(t, r, "acme")
	q := "url=" + url.QueryEscape(target) + "&format=json&token=" + token

	rec := scrapeURL(t, r, q)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("fetch failure: status %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := remaining(t, cm, token); got != cmManager.MaxRequests {
		t.Errorf("failed scrape was charged: %d left, want %d", got, cmManager.MaxRequests)
	}

	// A slot must be freed by the failure too, or the customer would be capped
	// for the rest of the process lifetime.
	inFlight, err := cm.InFlight(token)
	if err != nil {
		t.Fatalf("in flight: %v", err)
	}
	if inFlight != 0 {
		t.Errorf("failed scrape leaked a slot: in flight %d, want 0", inFlight)
	}
}

func TestTimeoutScrapeIsRefunded(t *testing.T) {
	allowLoopbackTarget(t)
	origClient := newFetchClient
	// A client that always times out, standing in for a target that accepts the
	// connection and then stalls.
	newFetchClient = func() *http.Client {
		return &http.Client{Timeout: 50 * time.Millisecond}
	}
	t.Cleanup(func() { newFetchClient = origClient })

	_, cm, r := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	token := newCustomer(t, r, "acme")
	rec := scrapeURL(t, r, "url="+url.QueryEscape(srv.URL)+"&format=json&token="+token)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout: status %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}
	if got := remaining(t, cm, token); got != cmManager.MaxRequests {
		t.Errorf("timeout was charged: %d left, want %d", got, cmManager.MaxRequests)
	}
}

func TestInFlightCapIsReportedAs429(t *testing.T) {
	allowLoopbackTarget(t)
	_, _, r := newTestHandler(t)

	release := make(chan struct{})
	// A buffered channel rather than a WaitGroup: the server is reused for the
	// follow-up requests below, so the handler can legitimately be entered more
	// than MaxInFlight times in total.
	started := make(chan struct{}, cmManager.MaxInFlight)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Write([]byte("slow"))
	}))
	defer srv.Close()

	token := newCustomer(t, r, "acme")
	q := "url=" + url.QueryEscape(srv.URL) + "&format=json&token=" + token

	var wg sync.WaitGroup
	codes := make(chan int, cmManager.MaxInFlight)
	for i := 0; i < cmManager.MaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- scrapeURL(t, r, q).Code
		}()
	}
	for i := 0; i < cmManager.MaxInFlight; i++ {
		<-started
	}

	// The next request is over the cap: 429 with a retry hint, not 402.
	rec := scrapeURL(t, r, q)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over cap: status %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("over cap: missing Retry-After header")
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("in-flight scrape: status %d, want %d", code, http.StatusOK)
		}
	}

	// With the slots freed again the customer is servable.
	if rec := scrapeURL(t, r, q); rec.Code != http.StatusOK {
		t.Errorf("after cap cleared: status %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestConcurrentBurstNeverOverspends(t *testing.T) {
	allowLoopbackTarget(t)
	_, cm, r := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// A balance far below the burst size, so most requests must be refused.
	seeded, err := cm.SeedDemoCustomer("burst", "burst-token", 5)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var ok, limited, refused, other int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch scrapeURL(t, r, "url="+url.QueryEscape(srv.URL)+"&format=html&token="+seeded.ID).Code {
			case http.StatusOK:
				atomic.AddInt64(&ok, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&limited, 1)
			case http.StatusPaymentRequired:
				atomic.AddInt64(&refused, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()

	// Every success costs exactly one unit and nothing else does. The balance is
	// not necessarily zero at the end: requests refused with 429 or 402 were
	// turned away without charging, so a burst can end with credit left over.
	if got, want := remaining(t, cm, seeded.ID), uint(5)-uint(ok); got != want {
		t.Errorf("balance after burst: %d, want %d (%d successes on a balance of 5)", got, want, ok)
	}
	if ok > 5 {
		t.Errorf("served %d scrapes on a balance of 5", ok)
	}
	if other != 0 {
		t.Errorf("%d requests returned an unexpected status", other)
	}
	if ok+limited+refused != 40 {
		t.Errorf("unaccounted responses: %d ok, %d limited, %d refused", ok, limited, refused)
	}
}

func TestRefillIsAdminOnlyAndNotImplemented(t *testing.T) {
	_, cm, r := newTestHandler(t)
	token := newCustomer(t, r, "acme")

	// Unauthenticated.
	req := httptest.NewRequest(http.MethodPost, "/v1/users/"+token+"/refill", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no admin token: status %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Authenticated, but the endpoint is not built yet.
	req = httptest.NewRequest(http.MethodPost, "/v1/users/"+token+"/refill", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer adm")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("refill: status %d, want %d", rec.Code, http.StatusNotImplemented)
	}

	// A stub that reported success would be worse than one that refuses.
	if got := remaining(t, cm, token); got != cmManager.MaxRequests {
		t.Errorf("refill stub changed the balance: %d, want %d", got, cmManager.MaxRequests)
	}
}

func TestNoPingRoute(t *testing.T) {
	_, _, r := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/ping: status %d, want %d", rec.Code, http.StatusNotFound)
	}
}
