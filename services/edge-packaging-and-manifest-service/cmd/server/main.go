// Command server runs the Edge Routing & Packaging Engine's HTTP server,
// binding the two public endpoints (personalized manifest generation and
// JIT chunk packaging) implemented in package httpapi, plus /healthz and
// /metrics. Alongside the primary HTTP/1.1 (net/http) listener, it also
// starts a second QUIC/HTTP-3 listener (package quicedge) serving the
// identical handler, so both transports are available side by side — see
// the README's "QUIC/HTTP-3 edge termination" section and
// internal/quicedge's package doc for why that matters for this service
// and what is/isn't real about the accompanying congestion-control story.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/httpapi"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/quicedge"
)

// defaultListenAddress is the address this service's HTTP/1.1 listener
// binds to when the EDGE_PACKAGING_LISTEN_ADDR environment variable is not
// set.
const defaultListenAddress = ":8080"

// defaultHttp3ListenAddress is the UDP address this service's QUIC/HTTP-3
// listener binds to when the EDGE_PACKAGING_HTTP3_LISTEN_ADDR environment
// variable is not set. It deliberately uses a different port number than
// defaultListenAddress: QUIC runs over UDP and cannot share a TCP listen
// socket with the HTTP/1.1 server, and using a distinct port (rather than
// number-sharing across protocols) avoids any ambiguity about which
// listener a given port number refers to.
const defaultHttp3ListenAddress = ":8443"

// main wires up structured logging, builds the service's http.ServeMux via
// httpapi.NewEdgePackagingHttpServeMux, and starts serving it over both
// plain HTTP/1.1 (production-sane read/write timeouts) and QUIC/HTTP-3
// (self-signed dev TLS certificate — see internal/quicedge's package doc
// for the production-certificate caveat) concurrently. EDGE_PACKAGING_DISABLE_HTTP3=true
// disables the HTTP/3 listener entirely, for environments where UDP is
// unavailable or undesired.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	listenAddress := os.Getenv("EDGE_PACKAGING_LISTEN_ADDR")
	if listenAddress == "" {
		listenAddress = defaultListenAddress
	}

	mux := httpapi.NewEdgePackagingHttpServeMux(logger)

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)

	go func() {
		logger.Info("starting edge-packaging-and-manifest-service HTTP/1.1 listener", "listenAddress", listenAddress)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	var http3Server interface{ Close() error }
	if os.Getenv("EDGE_PACKAGING_DISABLE_HTTP3") != "true" {
		http3ListenAddress := os.Getenv("EDGE_PACKAGING_HTTP3_LISTEN_ADDR")
		if http3ListenAddress == "" {
			http3ListenAddress = defaultHttp3ListenAddress
		}

		devCert, err := quicedge.GenerateSelfSignedDevelopmentTlsCertificate()
		if err != nil {
			logger.Error("failed to generate self-signed dev TLS certificate for HTTP/3 listener", "error", err)
			os.Exit(1)
		}

		http3Srv := quicedge.NewHttp3EdgeTerminationServer(quicedge.Http3EdgeTerminationServerOptions{
			ListenAddress:  http3ListenAddress,
			TlsCertificate: devCert,
			Handler:        mux,
		})
		http3Server = http3Srv

		go func() {
			logger.Info("starting edge-packaging-and-manifest-service QUIC/HTTP-3 listener",
				"listenAddress", http3ListenAddress,
				"tlsCertificate", "self-signed (development only; see internal/quicedge package doc)",
			)
			if err := http3Srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	} else {
		logger.Info("HTTP/3 listener disabled via EDGE_PACKAGING_DISABLE_HTTP3=true")
		errCh <- nil
	}

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, closing listeners")
	case err := <-errCh:
		if err != nil {
			logger.Error("a listener exited with an error", "error", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP/1.1 listener shutdown error", "error", err)
	}
	if http3Server != nil {
		if err := http3Server.Close(); err != nil {
			logger.Error("HTTP/3 listener shutdown error", "error", err)
		}
	}
}
