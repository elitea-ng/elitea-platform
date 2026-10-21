package llmproxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// NewMTLSTransport builds an HTTP/1.1 transport that presents the given client
// certificate and verifies the peer against caFile.
//
// No elitea-main code reaches the LLM gateway with it any more (GatewayClient
// is gRPC). The provider host hops (internal/providerhost) still use it.
// TODO(#1359 U6): delete it when those hops move to gRPC.
func NewMTLSTransport(certFile, keyFile, caFile, serverName string) (*http.Transport, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caPEM, err := os.ReadFile(caFile) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, fmt.Errorf("read ca bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from ca bundle %q", caFile)
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		// No ResponseHeaderTimeout / WriteTimeout: SSE responses are long-lived.
		TLSHandshakeTimeout: 10 * time.Second,
	}, nil
}
