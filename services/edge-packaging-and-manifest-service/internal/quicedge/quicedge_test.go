package quicedge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// TestGenerateSelfSignedDevelopmentTlsCertificateProducesAUsableCertificate
// verifies the generated certificate parses back as a valid X.509
// certificate/key pair usable in a tls.Config, and is currently valid
// (NotBefore <= now <= NotAfter).
func TestGenerateSelfSignedDevelopmentTlsCertificateProducesAUsableCertificate(t *testing.T) {
	cert, err := GenerateSelfSignedDevelopmentTlsCertificate()
	if err != nil {
		t.Fatalf("GenerateSelfSignedDevelopmentTlsCertificate returned error: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected at least one DER certificate in the chain")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("failed to parse generated certificate: %v", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		t.Errorf("expected the generated certificate to be currently valid, got NotBefore=%v NotAfter=%v now=%v", leaf.NotBefore, leaf.NotAfter, now)
	}
	if leaf.Subject.CommonName != "localhost" {
		t.Errorf("expected CommonName %q, got %q", "localhost", leaf.Subject.CommonName)
	}
}

// TestHttp3EdgeTerminationServerServesHttp3Requests is the required QUIC
// listener smoke test: it starts a real *http3.Server on an ephemeral UDP
// port using the self-signed dev certificate, issues a real HTTP/3 request
// against it with an http3.Transport client (TLS verification disabled,
// matching the documented self-signed-dev-cert caveat), and verifies the
// same handler this service wires onto its HTTP/1.1 listener answers
// correctly over QUIC too.
func TestHttp3EdgeTerminationServerServesHttp3Requests(t *testing.T) {
	cert, err := GenerateSelfSignedDevelopmentTlsCertificate()
	if err != nil {
		t.Fatalf("GenerateSelfSignedDevelopmentTlsCertificate returned error: %v", err)
	}

	handler := http.NewServeMux()
	handler.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-via-http3"))
	})

	// Bind an ephemeral UDP port up front (port 0) so the test does not
	// race with other tests/processes for a fixed port, then hand that
	// concrete address to the server.
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to reserve an ephemeral UDP port: %v", err)
	}
	listenAddr := udpConn.LocalAddr().String()
	udpConn.Close()

	server := NewHttp3EdgeTerminationServer(Http3EdgeTerminationServerOptions{
		ListenAddress:  listenAddr,
		TlsCertificate: cert,
		Handler:        handler,
	})

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- server.ListenAndServe()
	}()
	t.Cleanup(func() {
		_ = server.Close()
	})

	transport := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // self-signed dev certificate, see package doc
		},
	}
	defer transport.Close()

	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	requestURL := fmt.Sprintf("https://%s/healthz", listenAddr)

	// The QUIC listener takes a moment to become ready to accept
	// connections after ListenAndServe is kicked off in the background
	// goroutine above; retry briefly instead of introducing a fixed sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var lastErr error
	var respBody string
	for {
		select {
		case err := <-serveErrCh:
			t.Fatalf("HTTP/3 server exited early: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			t.Fatalf("failed to construct request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if resp.StatusCode != http.StatusOK {
				lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			} else {
				respBody = string(bodyBytes)
				lastErr = nil
			}
			if lastErr == nil {
				goto success
			}
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			t.Fatalf("HTTP/3 request never succeeded within timeout, last error: %v", lastErr)
		case <-time.After(50 * time.Millisecond):
		}
	}

success:
	if respBody != "ok-via-http3" {
		t.Errorf("expected response body %q, got %q", "ok-via-http3", respBody)
	}
}
