package natstest

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
)

// Probe opens one connection the way any client does — read the server's
// INFO, upgrade to TLS presenting identity's certificate (or none when
// identity is ""), send CONNECT and PING — and reports whether the server
// accepted it. It speaks the wire protocol directly so the refusal tests do
// not depend on a client library's retry and error-mapping behaviour.
func (s *Server) Probe(identity string) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	info, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read INFO: %w", err)
	}
	if !strings.HasPrefix(info, "INFO ") {
		return fmt.Errorf("expected INFO, got %q", info)
	}
	ca := natsconn.Material{CAFile: s.Material(natsconn.IdentityMain).CAFile}
	pool, err := ca.RootCAs()
	if err != nil {
		return err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "127.0.0.1"}
	if identity != "" {
		cert, err := s.Material(identity).ClientCertificate()
		if err != nil {
			return err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	tc := tls.Client(conn, cfg)
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	if _, err := tc.Write([]byte("CONNECT {\"verbose\":false,\"pedantic\":false,\"tls_required\":true,\"protocol\":1}\r\nPING\r\n")); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}
	line, err := bufio.NewReader(tc).ReadString('\n')
	if err != nil {
		// A server that refuses a certificate during the TLS 1.3 handshake
		// reports it on the first read, not in Handshake.
		return fmt.Errorf("read reply: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "PONG" {
		return nil
	}
	return errors.New(line)
}

// ProbePlain connects without upgrading to TLS and sends CONNECT: what a
// pod that dials nats:// does. A secured server must refuse it.
func (s *Server) ProbePlain() error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil {
		return fmt.Errorf("read INFO: %w", err)
	}
	if _, err := conn.Write([]byte("CONNECT {\"verbose\":false,\"pedantic\":false,\"protocol\":1}\r\nPING\r\n")); err != nil {
		return err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "PONG" {
		return nil
	}
	return errors.New(line)
}
