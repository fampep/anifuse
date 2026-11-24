// Command api runs the AniFuse resolve server.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fampep/anifuse/internal/httpapi"
	"github.com/fampep/anifuse/internal/providers"
)

const defaultPort = "8080"

func main() {
	port := envOr("PORT", defaultPort)
	if port[0] == ':' {
		port = port[1:]
	}

	registry := providers.NewRegistry()
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           httpapi.NewServer(registry).Handler(),
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("AniFuse listening on :%s (%d providers)", port, len(registry.Providers()))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
