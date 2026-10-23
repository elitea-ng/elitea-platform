// Command elitea-grpc-health checks grpc.health.v1 on one of elitea-main's
// internal mTLS gRPC listeners — in practice the platform listener (:9446,
// #1359), which serves gRPC only and so cannot be probed with curl or
// `openssl s_client` and an HTTP request line any more.
//
// It presents a client certificate (the listener requires one that chains to
// the runtime CA), verifies the server against the runtime CA alone under
// --server-name, and exits 0 only on SERVING:
//
//	elitea-grpc-health --addr elitea-main:9446 --ca /m/runtime-ca.crt \
//	    --cert /m/agent-worker-client.crt --key /m/agent-worker-client.key
//
// Exit codes: 0 SERVING, 1 any other answer or failure, 2 usage.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("elitea-grpc-health", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("addr", "", "host:port of the gRPC listener")
	caFile := flags.String("ca", "", "PEM bundle that verifies the server (the runtime CA)")
	certFile := flags.String("cert", "", "client certificate chain (PEM)")
	keyFile := flags.String("key", "", "client private key (PEM)")
	serverName := flags.String("server-name", "elitea-main", "name the server certificate must carry")
	service := flags.String("service", "", "service to check; empty checks the server as a whole")
	timeout := flags.Duration("timeout", 10*time.Second, "deadline for the whole check")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *address == "" || *caFile == "" || (*certFile == "") != (*keyFile == "") || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "elitea-grpc-health: --addr and --ca are required; --cert and --key go together")
		return 2
	}
	clientTLS, err := loadClientTLS(*caFile, *certFile, *keyFile, *serverName)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "elitea-grpc-health: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	status, err := check(ctx, *address, clientTLS, *service)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "elitea-grpc-health: %s: %v\n", *address, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, status.String())
	if status != healthv1.HealthCheckResponse_SERVING {
		return 1
	}
	return 0
}

func loadClientTLS(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA file holds no certificate")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return config, nil
}

func check(ctx context.Context, address string, clientTLS *tls.Config, service string) (healthv1.HealthCheckResponse_ServingStatus, error) {
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	response, err := healthv1.NewHealthClient(connection).Check(ctx, &healthv1.HealthCheckRequest{Service: service})
	if err != nil {
		return 0, err
	}
	return response.GetStatus(), nil
}
