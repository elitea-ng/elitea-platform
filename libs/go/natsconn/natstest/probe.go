package natstest

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	conn, err := s.probe(identity, "")
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

// Account reports the account the server put identity's connection in, read
// from the server's own connection list (/connz?auth=true) while a probe
// connection is held open. It is the answer verify_and_map gave, rather
// than one a client library reports about itself.
func (s *Server) Account(identity string) (string, error) {
	name := "natstest-account-probe-" + identity
	conn, err := s.probe(identity, name)
	if conn != nil {
		defer func() { _ = conn.Close() }()
	}
	if err != nil {
		return "", err
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/connz?auth=true&limit=1024", s.httpPort)) //nolint:gosec,noctx // a local test server
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var connz struct {
		Connections []struct {
			Name    string `json:"name"`
			Account string `json:"account"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&connz); err != nil {
		return "", err
	}
	for _, c := range connz.Connections {
		if c.Name == name {
			return c.Account, nil
		}
	}
	return "", fmt.Errorf("connection %s is not in the server's connection list", name)
}

// probe opens one connection and returns it open (nil on a refusal).
func (s *Server) probe(identity, name string) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()
	conn := raw
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	info, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read INFO: %w", err)
	}
	if !strings.HasPrefix(info, "INFO ") {
		return nil, fmt.Errorf("expected INFO, got %q", info)
	}
	ca := natsconn.Material{CAFile: s.Material(natsconn.IdentityMain).CAFile}
	pool, err := ca.RootCAs()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "127.0.0.1"}
	if identity != "" {
		cert, err := s.Material(identity).ClientCertificate()
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	tc := tls.Client(conn, cfg)
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	connect, err := json.Marshal(map[string]any{"verbose": false, "pedantic": false, "tls_required": true, "protocol": 1, "name": name})
	if err != nil {
		return nil, err
	}
	if _, err := tc.Write([]byte("CONNECT " + string(connect) + "\r\nPING\r\n")); err != nil {
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	line, err := bufio.NewReader(tc).ReadString('\n')
	if err != nil {
		// A server that refuses a certificate during the TLS 1.3 handshake
		// reports it on the first read, not in Handshake.
		return nil, fmt.Errorf("read reply: %w", err)
	}
	line = strings.TrimSpace(line)
	if line != "PONG" {
		return nil, errors.New(line)
	}
	ok = true
	return tc, nil
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
