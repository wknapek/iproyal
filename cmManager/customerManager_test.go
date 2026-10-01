package cmManager

import (
	"errors"
	"sync"
	"testing"
)

func TestAllowanceStartsAtInitCustReq(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.CreateCustomer("acme")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if c.noRequests != MaxRequests {
		t.Fatalf("noRequests = %d, want %d", c.noRequests, MaxRequests)
	}
	if c.ID == "" {
		t.Fatal("customer ID is empty")
	}
}

func TestIDIsTheToken(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.CreateCustomer("acme")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if _, _, err := cm.Acquire(c.ID); err != nil {
		t.Fatalf("customer ID should authenticate as a token, got %v", err)
	}
}

func TestTokensAreUnique(t *testing.T) {
	cm := NewCustomerManager()
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		c, err := cm.CreateCustomer("acme")
		if err != nil {
			t.Fatalf("CreateCustomer: %v", err)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate token generated: %s", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestAllowanceIsEnforced(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.CreateCustomer("acme")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	for i := uint(0); i < MaxRequests; i++ {
		_, rem, err := cm.Acquire(c.ID)
		if err != nil {
			t.Fatalf("request %d rejected (%v), allowance should still have room", i+1, err)
		}
		if want := MaxRequests - i - 1; rem != want {
			t.Fatalf("request %d: remaining = %d, want %d", i+1, rem, want)
		}
		// Release immediately so this test isolates the balance limit rather than
		// tripping the in-flight cap.
		cm.Release(c.ID, true)
	}
	if _, _, err := cm.Acquire(c.ID); !errors.Is(err, ErrNoRequests) {
		t.Fatalf("request %d: err = %v, want ErrNoRequests", MaxRequests+1, err)
	}
}

func TestExhaustionIsDistinctFromBadToken(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")
	for range uint(MaxRequests) {
		cm.Acquire(c.ID)
		cm.Release(c.ID, true)
	}
	if _, _, err := cm.Acquire(c.ID); !errors.Is(err, ErrNoRequests) {
		t.Fatalf("exhausted token: err = %v, want ErrNoRequests", err)
	}
	if _, _, err := cm.Acquire("garbage"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("unknown token: err = %v, want ErrUnknownToken", err)
	}
}

func TestAllowanceIsPerCustomer(t *testing.T) {
	cm := NewCustomerManager()
	a, _ := cm.CreateCustomer("a")
	b, _ := cm.CreateCustomer("b")

	for i := uint(0); i < MaxRequests; i++ {
		cm.Acquire(a.ID)
		cm.Release(a.ID, true)
	}
	if _, _, err := cm.Acquire(a.ID); !errors.Is(err, ErrNoRequests) {
		t.Fatalf("a should be exhausted, got %v", err)
	}
	if _, _, err := cm.Acquire(b.ID); err != nil {
		t.Fatalf("b should be unaffected by a's usage, got %v", err)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	cm := NewCustomerManager()
	if _, _, err := cm.Acquire("not-a-real-token"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("err = %v, want ErrUnknownToken", err)
	}
	if _, err := cm.Remaining("not-a-real-token"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("Remaining err = %v, want ErrUnknownToken", err)
	}
}

// Many concurrent acquires against a small balance must never drive it negative,
// and the balance must end up exactly at zero once all successful ones are charged.
func TestConcurrentRequestsCannotExceedAllowance(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.SeedDemoCustomer("acme", "concurrent-token", 20)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	const goroutines = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted, capped, broke := 0, 0, 0
	peakInFlight := 0
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := cm.Acquire(c.ID)
			mu.Lock()
			switch {
			case err == nil:
				granted++
				if n, _ := cm.InFlight(c.ID); n > peakInFlight {
					peakInFlight = n
				}
			case errors.Is(err, ErrTooManyInFlight):
				capped++
			default:
				broke++
			}
			mu.Unlock()
			if err == nil {
				cm.Release(c.ID, true)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted > 20 {
		t.Fatalf("granted %d, must never exceed the balance of 20", granted)
	}
	if granted+capped+broke != goroutines {
		t.Fatalf("accounted for %d of %d requests", granted+capped+broke, goroutines)
	}
	if peakInFlight > MaxInFlight {
		t.Fatalf("peak in-flight = %d, exceeded cap %d", peakInFlight, MaxInFlight)
	}
	if rem, _ := cm.Remaining(c.ID); rem != 0 {
		t.Fatalf("remaining = %d, want 0 after %d successful scrapes", rem, granted)
	}
	if inflight, _ := cm.InFlight(c.ID); inflight != 0 {
		t.Fatalf("in-flight = %d, want 0 after all requests completed", inflight)
	}
}

func TestRefundRestoresReservedRequest(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.CreateCustomer("acme")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	cm.Acquire(c.ID)
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests-1 {
		t.Fatalf("after acquire: remaining = %d, want %d", rem, MaxRequests-1)
	}
	cm.Release(c.ID, false)
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests {
		t.Fatalf("after failed release: remaining = %d, want %d", rem, MaxRequests)
	}
}

// A successful release must not refund: the unit was spent on a delivered result.
func TestSuccessfulReleaseKeepsTheCharge(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	cm.Acquire(c.ID)
	cm.Release(c.ID, true)
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests-1 {
		t.Fatalf("remaining = %d, want %d: a successful scrape must stay charged", rem, MaxRequests-1)
	}
}

// A failed release with no prior acquire must not manufacture allowance.
func TestReleaseCannotInflateBalance(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	cm.Release(c.ID, false)
	cm.Release(c.ID, false)
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests {
		t.Fatalf("remaining = %d, want %d: release with no acquire inflated the balance", rem, MaxRequests)
	}

	cm.Acquire(c.ID)
	cm.Release(c.ID, false)
	cm.Release(c.ID, false)
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests {
		t.Fatalf("remaining = %d, want %d: duplicate release inflated the balance", rem, MaxRequests)
	}
}

func TestReleaseUnknownTokenIsNoop(t *testing.T) {
	cm := NewCustomerManager()
	cm.Release("not-a-real-token", false)
}

// Acquire then release as a failure, repeated, must not change the balance, so a
// customer whose scrapes keep failing never drifts toward exhaustion.
func TestRepeatedFailedScrapesAreBalanceNeutral(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	for i := 0; i < 500; i++ {
		if _, _, err := cm.Acquire(c.ID); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		cm.Release(c.ID, false)
	}
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests {
		t.Fatalf("remaining = %d, want %d", rem, MaxRequests)
	}
}

// A customer that mixes successful and failed scrapes is charged only for the
// successful ones.
func TestOnlySuccessfulScrapesAreCharged(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	const failures = 30
	const successes = 5
	for i := 0; i < failures; i++ {
		cm.Acquire(c.ID)
		cm.Release(c.ID, false)
	}
	for i := 0; i < successes; i++ {
		if _, _, err := cm.Acquire(c.ID); err != nil {
			t.Fatalf("success %d: %v", i, err)
		}
		cm.Release(c.ID, true)
	}
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests-successes {
		t.Fatalf("remaining = %d, want %d: only %d successes should be charged",
			rem, MaxRequests-successes, successes)
	}
}

// Mixing failures with real usage under concurrency must never inflate or invert
// the balance.
func TestConcurrentAcquireReleaseStaysWithinLimits(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	const goroutines = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(succeeds bool) {
			defer wg.Done()
			<-start
			if _, _, err := cm.Acquire(c.ID); err == nil {
				cm.Release(c.ID, succeeds)
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()

	if inflight, err := cm.InFlight(c.ID); err != nil || inflight != 0 {
		t.Fatalf("InFlight = %d (err=%v), want 0: every slot must be released", inflight, err)
	}
	rem, err := cm.Remaining(c.ID)
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if rem > MaxRequests {
		t.Fatalf("remaining = %d, must never exceed %d", rem, MaxRequests)
	}
}

func TestInFlightCapIsEnforced(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	for i := 0; i < MaxInFlight; i++ {
		if _, _, err := cm.Acquire(c.ID); err != nil {
			t.Fatalf("slot %d should be granted, got %v", i+1, err)
		}
	}
	if inflight, _ := cm.InFlight(c.ID); inflight != MaxInFlight {
		t.Fatalf("InFlight = %d, want %d", inflight, MaxInFlight)
	}

	if _, _, err := cm.Acquire(c.ID); !errors.Is(err, ErrTooManyInFlight) {
		t.Fatalf("request %d: err = %v, want ErrTooManyInFlight", MaxInFlight+1, err)
	}

	// A rejected over-cap request must not have touched the balance.
	if rem, _ := cm.Remaining(c.ID); rem != MaxRequests-MaxInFlight {
		t.Fatalf("remaining = %d, want %d: over-cap rejection consumed balance", rem, MaxRequests-MaxInFlight)
	}
}

func TestInFlightCapIsPerCustomer(t *testing.T) {
	cm := NewCustomerManager()
	a, _ := cm.CreateCustomer("a")
	b, _ := cm.CreateCustomer("b")

	for i := 0; i < MaxInFlight; i++ {
		cm.Acquire(a.ID)
	}
	if _, _, err := cm.Acquire(a.ID); !errors.Is(err, ErrTooManyInFlight) {
		t.Fatalf("a should be at its cap, got %v", err)
	}
	if _, _, err := cm.Acquire(b.ID); err != nil {
		t.Fatalf("b should be unaffected by a's in-flight slots, got %v", err)
	}
}

// Releasing a slot must let the next request through, so a slow target cannot
// permanently lock a customer out.
func TestReleaseFreesInFlightSlot(t *testing.T) {
	cm := NewCustomerManager()
	c, _ := cm.CreateCustomer("acme")

	for i := 0; i < MaxInFlight; i++ {
		cm.Acquire(c.ID)
	}
	if _, _, err := cm.Acquire(c.ID); !errors.Is(err, ErrTooManyInFlight) {
		t.Fatalf("expected cap reached, got %v", err)
	}

	cm.Release(c.ID, false)
	if inflight, _ := cm.InFlight(c.ID); inflight != MaxInFlight-1 {
		t.Fatalf("InFlight = %d, want %d", inflight, MaxInFlight-1)
	}
	if _, _, err := cm.Acquire(c.ID); err != nil {
		t.Fatalf("a freed slot should be reusable, got %v", err)
	}
}

// Concurrency and balance are enforced together: a burst larger than both limits
// must be bounded by the tighter one and never drive either counter out of range.
func TestConcurrentBurstRespectsBothLimits(t *testing.T) {
	cm := NewCustomerManager()
	c, err := cm.SeedDemoCustomer("demo", "seeded-token", 10)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted, capped, broke := 0, 0, 0
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := cm.Acquire(c.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted++
			case errors.Is(err, ErrTooManyInFlight):
				capped++
			default:
				broke++
			}
		}()
	}
	close(start)
	wg.Wait()

	if inflight, _ := cm.InFlight(c.ID); inflight > MaxInFlight {
		t.Fatalf("InFlight = %d, exceeded cap %d", inflight, MaxInFlight)
	}
	if granted > 10 {
		t.Fatalf("granted %d, must not exceed the balance of 10", granted)
	}
	if granted+capped+broke != goroutines {
		t.Fatalf("accounted for %d of %d requests", granted+capped+broke, goroutines)
	}
}

func TestSeedDemoCustomer(t *testing.T) {
	cm := NewCustomerManager()
	cust, err := cm.SeedDemoCustomer("demo", "demo-token", 100)
	if err != nil {
		t.Fatalf("SeedDemoCustomer: %v", err)
	}
	if cust.ID != "demo-token" {
		t.Fatalf("ID = %q, want %q", cust.ID, "demo-token")
	}
	if rem, err := cm.Remaining("demo-token"); err != nil || rem != 100 {
		t.Fatalf("Remaining = %d (err=%v), want 100", rem, err)
	}
	// The seeded token must work as a real customer token.
	if _, _, err := cm.Acquire("demo-token"); err != nil {
		t.Fatalf("seeded token should authenticate, got %v", err)
	}
}

func TestSeedDemoCustomerRejectsBadInput(t *testing.T) {
	cm := NewCustomerManager()
	if _, err := cm.SeedDemoCustomer("demo", "", 100); err == nil {
		t.Error("empty token should be rejected")
	}
	if _, err := cm.SeedDemoCustomer("demo", "tok", MaxRequests+1); err == nil {
		t.Error("balance above the maximum should be rejected")
	}
	if _, err := cm.SeedDemoCustomer("demo", "tok", 100); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if _, err := cm.SeedDemoCustomer("demo", "tok", 100); err == nil {
		t.Error("duplicate token should be rejected")
	}
}
