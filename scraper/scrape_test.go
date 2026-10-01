package scraper

import (
	"context"
	"errors"
	"ipRoyal/cmManager"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// The fetch must give up on a target that accepts the connection and then stalls,
// rather than holding the customer's slot open indefinitely.
func TestFetchTimeoutOnStalledTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in short mode")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send headers, then hang without ever finishing the body.
		select {
		case <-r.Context().Done():
		case <-time.After(60 * time.Second):
		}
	}))
	defer srv.Close()

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}

	// A deadline well under the production 15s ceiling, so the test proves the
	// deadline is honored without waiting the full duration.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = Fetch(ctx, NewFetchClientFor(true), target)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetch of a stalled target should have failed")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fetch took %v, the deadline was not enforced", elapsed)
	}
}

// A hung target must not pin the customer's in-flight slot: once the scrape is
// released, the customer can make progress again.
func TestStalledScrapeFreesInFlightSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing test in short mode")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cm := cmManager.NewCustomerManager()
	cust, err := cm.SeedDemoCustomer("demo", "stall-token", 100)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	// Occupy every slot with requests that time out, then confirm they all drain.
	var wg sync.WaitGroup
	for i := 0; i < cmManager.MaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			target, _ := url.Parse(srv.URL)
			if _, err := Fetch(ctx, NewFetchClientFor(true), target); err == nil {
				t.Error("stalled fetch should have failed")
			}
			cm.Release(cust.ID, false)
		}()
	}
	wg.Wait()

	if inflight, err := cm.InFlight(cust.ID); err != nil || inflight != 0 {
		t.Fatalf("InFlight = %d (err=%v), want 0: slots must drain after timeouts", inflight, err)
	}
	if rem, _ := cm.Remaining(cust.ID); rem != 100 {
		t.Fatalf("remaining = %d, want 100: timed-out scrapes must not be charged", rem)
	}
}

// The in-flight cap must reject the surplus rather than queueing it, and the
// rejection must be distinguishable from being out of credit.
func TestInFlightCapRejectsSurplusWithoutCharging(t *testing.T) {
	cm := cmManager.NewCustomerManager()
	_, err := cm.SeedDemoCustomer("demo", "cap-token", 100)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	// Fill every slot.
	for i := 0; i < cmManager.MaxInFlight; i++ {
		if _, _, err := cm.Acquire("cap-token"); err != nil {
			t.Fatalf("slot %d: %v", i+1, err)
		}
	}
	balanceAfterFill, _ := cm.Remaining("cap-token")

	// Surplus requests are refused, and refusing them costs nothing.
	for i := 0; i < 50; i++ {
		_, rem, err := cm.Acquire("cap-token")
		if !errors.Is(err, cmManager.ErrTooManyInFlight) {
			t.Fatalf("surplus %d: err = %v, want errTooManyInFlight", i, err)
		}
		if rem != balanceAfterFill {
			t.Fatalf("surplus %d: remaining = %d, want %d", i, rem, balanceAfterFill)
		}
	}
	if inflight, _ := cm.InFlight("cap-token"); inflight != cmManager.MaxInFlight {
		t.Fatalf("InFlight = %d, want %d", inflight, cmManager.MaxInFlight)
	}
}

// The timeout must be applied to the client, not just the caller's context, so a
// target that trickles bytes after sending headers is still cut off.
func TestFetchClientHasFirmTimeout(t *testing.T) {
	client := NewFetchClient()
	if client.Timeout <= 0 {
		t.Fatal("http.Client.Timeout must be set so the body read is bounded")
	}
	if client.Timeout > 60*time.Second {
		t.Fatalf("http.Client.Timeout = %v, expected a firm sub-minute bound", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected an *http.Transport")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout must be set so a header-stalling target is cut off")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("IdleConnTimeout must be set so sockets do not outlive their request")
	}
}

// Over-cap and out-of-credit must be separately detectable, since a client
// responds to them differently.
func TestCapAndCreditErrorsAreDistinct(t *testing.T) {
	cm := cmManager.NewCustomerManager()
	if _, err := cm.SeedDemoCustomer("demo", "distinct-token", cmManager.MaxInFlight); err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	for i := 0; i < cmManager.MaxInFlight; i++ {
		cm.Acquire("distinct-token")
	}
	// At the cap with balance left: over-cap wins.
	if _, _, err := cm.Acquire("distinct-token"); !errors.Is(err, cmManager.ErrTooManyInFlight) {
		t.Fatalf("err = %v, want errTooManyInFlight", err)
	}

	// Drain the slots and the balance, then the customer is out of credit.
	for i := 0; i < cmManager.MaxInFlight; i++ {
		cm.Release("distinct-token", true)
	}
	if rem, _ := cm.Remaining("distinct-token"); rem != 0 {
		t.Fatalf("remaining = %d, want 0", rem)
	}
	if _, _, err := cm.Acquire("distinct-token"); !errors.Is(err, cmManager.ErrNoRequests) {
		t.Fatalf("err = %v, want errNoRequests", err)
	}
}

// A scrape rejected for any reason must never leave a slot held, or a customer
// could lock themselves out through no fault of their own.
func TestRejectedScrapeReleasesNothing(t *testing.T) {
	cm := cmManager.NewCustomerManager()
	_, err := cm.SeedDemoCustomer("demo", "reject-token", 100)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	// A bad token and a rejected target both fail before a slot is taken.
	if _, _, err := cm.Acquire("nope"); err == nil {
		t.Error("unknown token should be rejected")
	}
	if inflight, _ := cm.InFlight("reject-token"); inflight != 0 {
		t.Fatalf("InFlight = %d, want 0", inflight)
	}
	if rem, _ := cm.Remaining("reject-token"); rem != 100 {
		t.Fatalf("remaining = %d, want 100", rem)
	}
}

// The guard is the SSRF boundary, so every range that could reach internal
// infrastructure or the cloud metadata service has to be rejected.
func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", // private
		"169.254.169.254",    // link local, AWS/GCP metadata
		"0.0.0.0", "0.1.2.3", // unspecified and 0.0.0.0/8
		"224.0.0.1", "ff02::1", // multicast
		"::1", "fe80::1", "::",
	}
	for _, s := range blocked {
		if IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = true, want false", s)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		if !IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = false, want true", s)
		}
	}

	if IsPublicIP(nil) {
		t.Error("IsPublicIP(nil) = true, want false")
	}
}

func TestIsTimeout(t *testing.T) {
	// A context deadline is the timeout signal the handler relies on to
	// distinguish 504 from 502.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()

	if !IsTimeout(ctx.Err()) {
		t.Error("IsTimeout(context.DeadlineExceeded) = false, want true")
	}
	if IsTimeout(errors.New("connection refused")) {
		t.Error("IsTimeout(plain error) = true, want false")
	}
	if IsTimeout(nil) {
		t.Error("IsTimeout(nil) = true, want false")
	}
}
