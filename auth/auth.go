package auth

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

// AdminAuth rejects any request that does not present expected as a bearer token.
// The comparison is constant time so a caller cannot learn the token by measuring
// how long a rejection takes.
func AdminAuth(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := c.GetHeader("Authorization")
		if len(got) > 7 && got[:7] == "Bearer " {
			got = got[7:]
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing admin token"})
			return
		}
		c.Next()
	}
}

// RequireEnv returns the value of an environment variable, panicking if it is
// unset. Used for secrets the service cannot start without.
func RequireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(name + " env var is required")
	}
	return v
}
