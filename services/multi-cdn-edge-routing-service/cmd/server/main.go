// Command server runs the multi-cdn-edge-routing-service production HTTP
// server: the multi-CDN routing decision engine and predictive
// pre-fetching engine, exposed over net/http.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/cdnhealth"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/httpapi"
)

// syntheticHealthFeedInterval controls how often the demo server refreshes
// its simulated CDN health data. In a real deployment this would instead be
// however often the real monitoring pipeline pushes updates.
const syntheticHealthFeedInterval = 15 * time.Second

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	healthRegistry := cdnhealth.NewCdnHealthRegistry()
	healthRegistry.IngestHealthReportFeed(httpapi.BuildSyntheticCdnHealthFeed(time.Now()))

	server := httpapi.NewServer(healthRegistry)
	stopFeed := httpapi.StartPeriodicSyntheticHealthFeed(server, syntheticHealthFeedInterval)
	defer stopFeed()

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("multi-cdn-edge-routing-service listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()

	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
