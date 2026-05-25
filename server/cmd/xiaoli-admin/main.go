package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"xiaoli/server/internal/admin"
)

func main() {
	cfg := admin.LoadConfig()
	if cfg.SessionSecret == "" || len(cfg.SessionSecret) < 32 {
		log.Fatal("ADMIN_SESSION_SECRET must be at least 32 characters")
	}
	if cfg.LogtoEndpoint == "/" || cfg.LogtoAppID == "" || cfg.LogtoAppSecret == "" {
		log.Fatal("LOGTO_ENDPOINT, LOGTO_APP_ID and LOGTO_APP_SECRET are required")
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := admin.NewServer(cfg)
	server.StartBackground(context.Background())
	log.Printf("Xiaoli Go admin listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, server))
}
