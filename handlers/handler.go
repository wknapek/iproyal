package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ipRoyal/auth"
	"ipRoyal/cmManager"
	"ipRoyal/scraper"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	cm         *cmManager.CustomerManager
	adminToken string
}

func NewHandler(cm *cmManager.CustomerManager) *Handler {
	return &Handler{cm: cm, adminToken: auth.RequireEnv("ADMIN_TOKEN")}
}

// validateTarget and newFetchClient are indirections over the scraper package so
// tests can point the handler at a local server without the SSRF guard blocking
// the loopback address. The serving path always uses the real implementations.
var (
	validateTarget = scraper.ValidateTarget
	newFetchClient = scraper.NewFetchClient
)

func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/v1/users", auth.AdminAuth(h.adminToken), h.createUser)
	r.POST("/v1/users/:id/refill", auth.AdminAuth(h.adminToken), h.refill)
	r.GET("/v1/scrape", h.scrape)
}

// refill tops a customer's request allowance back up.
//
// TODO: not implemented yet. Decide and settle the following before wiring it to
// the store:
//   - body shape: absolute ("set to N") versus additive ("add N"), and whether
//     an empty body means "reset to INITCUSTREQ";
//   - whether the ceiling is INITCUSTREQ or cmManager.MaxRequests, and whether
//     an exhausted customer may be topped back up at all;
//   - whether a refill is audited, and by which caller identity.
//
// Until then this returns 501 rather than a silent success, so a client cannot
// mistake a no-op for a completed refill.
func (h *Handler) refill(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error":    "refill is not implemented yet",
		"customer": c.Param("id"),
	})
}

type createUserRequest struct {
	Name string `json:"name"`
}

type createUserResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Requests  uint      `json:"requests"`
	Message   string    `json:"message"`
}

func (h *Handler) createUser(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'name' is required"})
		return
	}
	if len(name) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'name' is too long (max 128 characters)"})
		return
	}

	cust, err := h.cm.CreateCustomer(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create user"})
		return
	}

	c.JSON(http.StatusCreated, createUserResponse{
		ID:        cust.ID,
		Name:      cust.Name,
		CreatedAt: cust.CreatedAt,
		Requests:  cust.Balance(),
		Message:   "the id is the token, use it as ?token=<id>. it is not retrievable later",
	})
}

func (h *Handler) scrape(c *gin.Context) {
	// Request shape is validated before authentication, so a malformed call is
	// rejected as a client error and never draws down a customer's allowance.
	rawURL := c.Query("url")
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query param 'url' is required"})
		return
	}

	format := c.DefaultQuery("format", "html")
	if format != "html" && format != "json" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format must be 'html' or 'json'"})
		return
	}

	target, err := validateTarget(rawURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	token := c.Query("token")
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "query param 'token' is required"})
		return
	}

	// Take one unit of the prepaid balance and one of the customer's in-flight
	// slots atomically. Release below gives the unit back if the scrape does not
	// succeed, so the customer is only charged for results that were returned.
	cust, remaining, err := h.cm.Acquire(token)
	switch {
	case errors.Is(err, cmManager.ErrTooManyInFlight):
		// 429: the customer is over its concurrency cap, not out of credit. This is
		// retryable, and the response says so, so a client can back off rather than
		// treat it as a hard failure.
		c.Header("Retry-After", "1")
		c.Header("X-Requests-Remaining", strconv.FormatUint(uint64(remaining), 10))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":         "too many concurrent scrapes for this customer",
			"max_in_flight": cmManager.MaxInFlight,
			"requests_left": remaining,
		})
		return
	case errors.Is(err, cmManager.ErrNoRequests):
		c.Header("X-Requests-Remaining", "0")
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":         "no requests left on this account",
			"requests_left": 0,
		})
		return
	case err != nil:
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), scraper.FetchTimeout)
	defer cancel()

	res, err := scraper.Fetch(ctx, newFetchClient(), target)
	if err != nil {
		h.cm.Release(token, false)
		if errors.Is(err, context.DeadlineExceeded) || scraper.IsTimeout(err) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "upstream request timed out"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch url: " + err.Error()})
		return
	}
	// The scrape succeeded, so the reserved unit stays spent and the slot is freed.
	h.cm.Release(token, true)

	c.Header("X-Requests-Remaining", strconv.FormatUint(uint64(remaining), 10))

	if format == "html" {
		contentType := res.ContentType
		if contentType == "" {
			contentType = "text/html; charset=utf-8"
		}
		if res.Truncated {
			contentType = "text/plain; charset=utf-8"
			c.Header("X-Scrape-Truncated", "true")
		}
		c.Data(res.StatusCode, contentType, res.Body)
		return
	}

	c.JSON(res.StatusCode, gin.H{
		"customer":      cust.Name,
		"requests_left": remaining,
		"url":           rawURL,
		"final_url":     res.FinalURL,
		"status_code":   res.StatusCode,
		"content_type":  res.ContentType,
		"size_bytes":    len(res.Body),
		"truncated":     res.Truncated,
		"duration_ms":   res.Duration.Milliseconds(),
		"fetched_at":    time.Now().UTC().Format(time.RFC3339),
		"content":       string(res.Body),
	})
}
