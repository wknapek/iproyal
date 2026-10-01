package main

import (
	"log"
	"os"
	"strconv"

	"ipRoyal/cmManager"
	"ipRoyal/handlers"

	"github.com/gin-gonic/gin"
)

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	cmMgr := cmManager.NewCustomerManager()
	seedDemoCustomer(cmMgr)

	han := handlers.NewHandler(cmMgr)
	han.RegisterRoutes(r)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if err := r.Run(addr); err != nil {
		panic(err)
	}
}

// seedDemoCustomer puts one usable account in the in-memory store so the service
// is usable immediately after start, with no create-user round trip. The token is
// read from DEMO_TOKEN so it can be supplied by a secret store rather than baked
// into a default. If it is unset the seed is skipped and only POST /v1/users can
// mint credentials.
func seedDemoCustomer(cm *cmManager.CustomerManager) {
	token := os.Getenv("DEMO_TOKEN")
	if token == "" {
		log.Printf("DEMO_TOKEN not set, skipping demo customer (use POST /v1/users to create one)")
		return
	}

	balance := uint(cmManager.MaxRequests)
	if raw := os.Getenv("DEMO_BALANCE"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			log.Fatalf("DEMO_BALANCE is not a valid number: %v", err)
		}
		if parsed > cmManager.MaxRequests {
			log.Fatalf("DEMO_BALANCE %d exceeds the maximum of %d", parsed, cmManager.MaxRequests)
		}
		balance = uint(parsed)
	}

	cust, err := cm.SeedDemoCustomer("demo", token, balance)
	if err != nil {
		log.Fatalf("could not seed demo customer: %v", err)
	}
	log.Printf("seeded demo customer %q with %d requests, max %d concurrent scrapes",
		cust.Name, cust.Balance(), cmManager.MaxInFlight)
}
