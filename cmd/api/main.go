// Command api runs the AniFuse resolve server.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fampep/anifuse/internal/httpapi"
	"github.com/fampep/anifuse/internal/providers"
)

const (
	defaultPort    = "8080"
	defaultTLSPort = "443"
)

func main() {
	port := envOr("PORT", defaultPort)
	if port[0] == ':' {
		port = port[1:]
	}
	demo := envBool("ANIFUSE_DEMO")

	registry := providers.NewRegistry()
	handler := httpapi.NewServer(registry, demo).Handler()

	servers := make([]*http.Server, 0, 2)
	servers = append(servers, &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	})

	// Optional HTTPS: when ANIFUSE_TLS_CERT and ANIFUSE_TLS_KEY name a
	// usable certificate pair, the same API is also served over TLS on
	// ANIFUSE_TLS_PORT (default 443). Plain HTTP on PORT stays up for
	// internal clients and for the ACME renewals that keep the pair fresh.
	if srv := tlsServer(handler); srv != nil {
		servers = append(servers, srv)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			scheme := "http"
			if srv.TLSConfig != nil {
				scheme = "https"
			}
			log.Printf("AniFuse %s on %s (%d providers)", scheme, srv.Addr, len(registry.Providers()))
			var err error
			if srv.TLSConfig != nil {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("server %s: %v", srv.Addr, err)
			}
		}(srv)
	}

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	wg.Wait()
}

// tlsServer builds the HTTPS listener, or nil when no certificate pair is
// configured or the pair cannot be loaded.
func tlsServer(handler http.Handler) *http.Server {
	cert := os.Getenv("ANIFUSE_TLS_CERT")
	key := os.Getenv("ANIFUSE_TLS_KEY")
	if cert == "" || key == "" {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		log.Printf("anifuse: cannot load TLS pair (%v); HTTPS disabled", err)
		return nil
	}
	tlsPort := envOr("ANIFUSE_TLS_PORT", defaultTLSPort)
	if tlsPort[0] == ':' {
		tlsPort = tlsPort[1:]
	}
	return &http.Server{
		Addr:    ":" + tlsPort,
		Handler: handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value := os.Getenv(name)
	return value == "1" || value == "true" || value == "yes"
}
