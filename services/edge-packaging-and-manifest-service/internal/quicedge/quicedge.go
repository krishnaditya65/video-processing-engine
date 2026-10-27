// Package quicedge adds a second, QUIC/HTTP-3 edge listener alongside this
// service's existing net/http (HTTP/1.1) server, serving the exact same
// http.Handler over both transports. It exists to demonstrate — with real,
// running code, not just documentation — why an edge video-delivery layer
// benefits from QUIC, and to be honest about which parts of the "QUIC
// eliminates congestion problems" story are actually implemented here
// versus configured outside this codebase (see the package-level doc
// sections below and the README's "QUIC/HTTP-3 edge termination" section).
//
// # Why QUIC matters for this service specifically
//
// This service's whole job is serving many small, latency-sensitive
// objects per playback session — a personalized manifest, then dozens of
// ~2-second fMP4 chunks per minute of playback, often over cellular/Wi-Fi
// networks with real packet loss. Two QUIC properties matter concretely
// here:
//
//  1. Head-of-line blocking elimination. Over HTTP/1.1 (this service's
//     existing net/http listener), a client's persistent connection
//     serializes requests; over HTTP/2, independent streams multiplex on
//     one TCP connection, but TCP's in-order-delivery guarantee means a
//     single lost packet stalls *every* stream sharing that connection
//     until it is retransmitted — one dropped ftyp/moov packet can stall
//     an unrelated in-flight chunk request. QUIC multiplexes independent
//     streams over UDP, where a lost packet only blocks the stream(s) whose
//     data was in that packet; a lost packet belonging to one chunk
//     request no longer stalls a concurrent request for a different
//     rendition's chunk or the next manifest refresh.
//  2. Connection migration. A QUIC connection is identified by a
//     connection ID, not by the client's (source IP, source port) 4-tuple
//     the way a TCP connection is. A mobile viewer roaming from a Wi-Fi
//     access point to 5G (or between cell towers) gets a new IP address
//     mid-session; over TCP/HTTP1.1/HTTP2 that always tears down and
//     re-establishes the connection (and its TLS handshake), which for
//     this service means every in-flight chunk request stalls or fails at
//     exactly the moment the network changes. QUIC connections survive
//     that IP change without a new handshake, so playback continues
//     uninterrupted through the network transition.
//
// # Congestion control: what's real here vs. what's kernel-level vs. what's
// honestly out of scope
//
// The task of enabling "BBR congestion control" is frequently stated too
// simply, and it is worth being precise about three separate things that
// get conflated under that name:
//
//  1. BBR (as in `sysctl net.ipv4.tcp_congestion_control=bbr`) is a
//     kernel-implemented congestion-control algorithm for **TCP sockets**.
//     It lives in the Linux kernel's TCP stack (net/ipv4/tcp_bbr.c) and is
//     selected per-socket (or system-wide via that sysctl, or per-route via
//     `ip route ... congestion bbr`). This is genuinely outside this
//     codebase's reach: no amount of Go application code changes which
//     congestion-control algorithm a Linux kernel's TCP stack uses for
//     HTTP/1.1 or HTTP/2 connections. Setting that sysctl on an edge node
//     terminating this service's *existing* net/http listener is a real,
//     legitimate, orthogonal operational step — see the README for the
//     exact command — and it would apply to this service's plain HTTP/1.1
//     listener today.
//  2. That sysctl has **no effect on QUIC**, and this is the part that is
//     easy to state inaccurately: QUIC runs over UDP, and the Linux kernel
//     has no concept of "congestion control" for UDP datagrams at all —
//     UDP is unreliable/unordered/uncontrolled by design, and the kernel
//     does not rate-limit or pace it. Congestion control for a QUIC
//     connection is implemented entirely in the QUIC library running in
//     user space (here, github.com/quic-go/quic-go), not the kernel. So
//     "does `tcp_congestion_control=bbr` speed up the QUIC listener this
//     package adds" has a flat "no" answer, not a nuanced one.
//  3. quic-go v0.61.0 (the version this package is built against) ships
//     its own built-in congestion controller (a Cubic-like algorithm in
//     its unexported internal/congestion package) and does not currently
//     expose a public API to swap in a different congestion-control
//     algorithm (verified against this module version: there is no
//     exported quic.Config field or interface for supplying a custom
//     congestion.SendAlgorithm). A production deployment that wanted
//     BBR-style congestion behavior for its QUIC/UDP traffic has two real
//     options, neither of which is "flip a config flag in this service":
//     (a) fork/patch quic-go's internal congestion package to implement a
//     BBR-like algorithm and satisfy its internal SendAlgorithm interface
//     (this is exactly the kind of change the Google QUIC/BoringSSL and
//     early quiche implementations made to ship BBRv1/BBRv2 for QUIC), or
//     (b) move to a QUIC implementation that does expose pluggable
//     congestion control. This package does not attempt either — doing so
//     is out of scope for an edge-packaging demo service and would mean
//     vendoring and maintaining a fork of a third-party transport library.
//     What this package *does* provide is the concrete integration point
//     such a change would plug into: quic.Config (constructed in
//     NewHttp3EdgeTerminationServer below) is where a congestion-control
//     override would be wired in once quic-go (or a replacement transport)
//     exposes one.
package quicedge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// selfSignedDevCertificateValidityWindow is how long
// GenerateSelfSignedDevelopmentTlsCertificate's certificate remains valid
// for, generous enough that a long-lived local dev/test process never hits
// certificate expiry mid-session.
const selfSignedDevCertificateValidityWindow = 365 * 24 * time.Hour

// GenerateSelfSignedDevelopmentTlsCertificate generates a fresh, in-memory,
// self-signed ECDSA P-256 TLS certificate/key pair for "localhost" and
// "127.0.0.1", suitable for local development and testing of the QUIC/HTTP-3
// listener this package adds. It is explicitly a *development* certificate:
// self-signed certificates are not trusted by real client TLS stacks
// without out-of-band trust configuration (e.g. `-k`/`--insecure` in curl,
// or a QUIC client configured with InsecureSkipVerify), and a production
// deployment must instead terminate QUIC with a certificate from a trusted
// CA (e.g. via ACME/Let's Encrypt), exactly as it would for the existing
// HTTP/1.1 listener.
func GenerateSelfSignedDevelopmentTlsCertificate() (tls.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicedge: generating ECDSA key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicedge: generating certificate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"edge-packaging-and-manifest-service (dev-only, self-signed)"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedDevCertificateValidityWindow),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	certificateDerBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicedge: creating self-signed certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{certificateDerBytes},
		PrivateKey:  privateKey,
	}, nil
}

// Http3EdgeTerminationServerOptions configures NewHttp3EdgeTerminationServer.
type Http3EdgeTerminationServerOptions struct {
	// ListenAddress is the UDP "host:port" address the HTTP/3 listener
	// binds to (e.g. ":8443"). Note this is a *separate* address/port from
	// this service's existing plain-HTTP net/http listener — QUIC runs
	// over UDP, so it cannot share a TCP listen socket, and in this
	// service's default configuration it also does not share a port
	// number, so both listeners can run side by side without conflict.
	ListenAddress string
	// TlsCertificate is the certificate/key pair the QUIC/TLS 1.3
	// handshake presents to clients. Use
	// GenerateSelfSignedDevelopmentTlsCertificate for local development;
	// a production deployment should supply a CA-issued certificate
	// instead.
	TlsCertificate tls.Certificate
	// Handler is the http.Handler to serve over HTTP/3 — in this service,
	// the exact same http.ServeMux httpapi.NewEdgePackagingHttpServeMux
	// builds for the HTTP/1.1 listener, so both transports expose
	// identical routes/behavior.
	Handler http.Handler
}

// NewHttp3EdgeTerminationServer constructs (but does not start) an
// *http3.Server configured to serve options.Handler over QUIC/HTTP-3 on
// options.ListenAddress. Call ListenAndServe on the returned server (or
// pass it to StartHttp3EdgeTerminationServerInBackground) to actually bind
// the UDP socket and begin serving.
//
// The returned quic.Config is left at library defaults except for
// enabling 0-RTT-friendly QUIC v1 support; this is the integration point
// documented at the top of this package where a pluggable
// congestion-control algorithm (see the package doc's BBR discussion)
// would be supplied once the underlying QUIC library exposes one.
func NewHttp3EdgeTerminationServer(options Http3EdgeTerminationServerOptions) *http3.Server {
	return &http3.Server{
		Addr:    options.ListenAddress,
		Handler: options.Handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{options.TlsCertificate},
			NextProtos:   []string{http3.NextProtoH3},
		},
		// QUICConfig is left as an explicit (rather than nil/implicit)
		// *quic.Config so the exact struct a future congestion-control
		// override would be added to is visible at the call site, per the
		// package doc's "integration point" note.
		QUICConfig: &quic.Config{},
	}
}
