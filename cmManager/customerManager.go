package cmManager

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	INITCUSTREQ = 100

	// MaxInFlight caps how many scrapes one customer may have outstanding at
	// once. Without it a single customer with a large balance could occupy every
	// connection in the fetch transport and starve everyone else, so the cap
	// bounds the concurrency one customer can demand of the service.
	MaxInFlight = 4
)

var errTokenCollision = errors.New("token collision, retry")

// Customer.ID is the customer's token. The token doubles as the identity, so a
// scrape request only has to carry one value. Tokens are server-generated and
// carry 256 bits of entropy; the lookup table is keyed by hash so that a dump of
// the manager does not hand out usable credentials.
//
// The balance and slot counters stay unexported: they may only be changed through
// Acquire and Release, which is what keeps the limits enforceable.
type Customer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`

	noRequests uint `json:"-"`
	inFlight   int  `json:"-"`
}

// Balance reports the customer's unused requests. Exported because the create-user
// response reports the starting balance.
func (c Customer) Balance() uint { return c.noRequests }

// MaxRequests is the ceiling a customer may be topped up to, independent of the
// balance they were created with.
const MaxRequests = 1000

type CustomerManager struct {
	mu          sync.Mutex
	byTokenHash map[[sha256.Size]byte]*Customer
}

func NewCustomerManager() *CustomerManager {
	return &CustomerManager{byTokenHash: make(map[[sha256.Size]byte]*Customer)}
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// CreateCustomer registers a customer under its freshly generated token, which
// becomes the customer's ID. The token is returned once and never stored.
func (cm *CustomerManager) CreateCustomer(name string) (Customer, error) {
	token, err := newToken()
	if err != nil {
		return Customer{}, err
	}
	return cm.register(name, token)
}

// SeedDemoCustomer inserts a customer with a caller-supplied token and balance,
// so the service comes up with a usable account instead of an empty store.
//
// The token is not generated, so it must come from a secret source. The caller is
// responsible for that; SeedDemoCustomer is a bootstrap aid, not a way to mint
// credentials.
func (cm *CustomerManager) SeedDemoCustomer(name, token string, balance uint) (Customer, error) {
	return cm.register(name, token, balance)
}

func (cm *CustomerManager) register(name, token string, balance ...uint) (Customer, error) {
	if token == "" {
		return Customer{}, errors.New("token must not be empty")
	}
	requests := uint(INITCUSTREQ)
	if len(balance) > 0 {
		if balance[0] > MaxRequests {
			return Customer{}, fmt.Errorf("balance %d exceeds the maximum of %d", balance[0], MaxRequests)
		}
		requests = balance[0]
	}

	sum := sha256.Sum256([]byte(token))
	c := &Customer{
		ID:         token,
		Name:       name,
		CreatedAt:  time.Now().UTC(),
		noRequests: requests,
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, exists := cm.byTokenHash[sum]; exists {
		return Customer{}, errTokenCollision
	}
	cm.byTokenHash[sum] = c
	return *c, nil
}

var (
	ErrUnknownToken    = errors.New("unknown token")
	ErrNoRequests      = errors.New("request allowance exhausted")
	ErrTooManyInFlight = errors.New("too many concurrent requests")
)

// Acquire admits a scrape for a customer: it takes one unit off the prepaid
// balance and one of the customer's in-flight slots, atomically, so a burst of
// concurrent requests cannot overshoot either limit. The caller must call Release
// exactly once when the request finishes, passing whether the scrape succeeded.
//
// Both the balance and the slot are taken up front on purpose. Checking a limit
// and then acting on it in two steps would let concurrent callers all observe
// room and then all proceed, driving the balance below zero or the in-flight count
// past the cap. Release refunds the unit and frees the slot, so the net effect is
// charge-per-success with a concurrency ceiling that holds at every instant.
func (cm *CustomerManager) Acquire(token string) (Customer, uint, error) {
	sum := sha256.Sum256([]byte(token))

	cm.mu.Lock()
	defer cm.mu.Unlock()
	c, ok := cm.byTokenHash[sum]
	if !ok {
		return Customer{}, 0, ErrUnknownToken
	}
	if c.inFlight >= MaxInFlight {
		return *c, c.noRequests, ErrTooManyInFlight
	}
	if c.noRequests == 0 {
		return *c, 0, ErrNoRequests
	}
	c.inFlight++
	c.noRequests--
	return *c, c.noRequests, nil
}

// Release ends a scrape started by Acquire. When success is false the reserved
// unit is refunded, so timeouts and transport errors cost the customer nothing.
//
// The refund is conditional on actually holding a slot, which makes a release
// without a matching acquire a no-op: otherwise a stray or duplicated Release would
// hand back credit that was never taken. Refunds also stop at MaxRequests so the
// balance cannot be pushed past the ceiling.
func (cm *CustomerManager) Release(token string, success bool) {
	sum := sha256.Sum256([]byte(token))

	cm.mu.Lock()
	defer cm.mu.Unlock()
	c, ok := cm.byTokenHash[sum]
	if !ok || c.inFlight == 0 {
		return
	}
	c.inFlight--
	if !success && c.noRequests < MaxRequests {
		c.noRequests++
	}
}

// InFlight reports how many of a customer's slots are currently held.
func (cm *CustomerManager) InFlight(token string) (int, error) {
	sum := sha256.Sum256([]byte(token))

	cm.mu.Lock()
	defer cm.mu.Unlock()
	c, ok := cm.byTokenHash[sum]
	if !ok {
		return 0, ErrUnknownToken
	}
	return c.inFlight, nil
}

// Remaining reports a customer's unused request allowance.
func (cm *CustomerManager) Remaining(token string) (uint, error) {
	sum := sha256.Sum256([]byte(token))

	cm.mu.Lock()
	defer cm.mu.Unlock()
	c, ok := cm.byTokenHash[sum]
	if !ok {
		return 0, ErrUnknownToken
	}
	return c.noRequests, nil
}

func (cm *CustomerManager) Count() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return len(cm.byTokenHash)
}
