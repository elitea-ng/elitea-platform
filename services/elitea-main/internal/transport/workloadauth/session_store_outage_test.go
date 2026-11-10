package workloadauth

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type recordedLog struct{ bytes.Buffer }

func (r *recordedLog) entries(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

var outageBinding = SessionBinding{WorkloadIdentity: "dns:worker.runtime.example", WorkloadSessionID: "session-7f3a", ProducerID: "producer-9c1d"}

func outagePeer() context.Context {
	return peerContext(&x509.Certificate{DNSNames: []string{"worker.runtime.example"}})
}

// FU-275 conditions 1, 2 and 4. Every outcome below refuses the workload: no
// identity is returned. Only a session registry that could not be read
// because the database is restarting or recovering is marked
// ErrSessionStoreUnavailable; an unknown, expired or revoked session (the
// registry answers ErrWorkloadUnauthorized), a foreign workload, an
// unverified peer and a permanent database fault are refused unmarked.
func TestAnUnreadableSessionStoreRefusesTheWorkloadAndIsMarkedRetryableOnlyThen(t *testing.T) {
	cases := []struct {
		name        string
		ctx         context.Context
		session     string
		verifierErr error
		wantMarked  bool
	}{
		{"database restarting (57P01)", outagePeer(), outageBinding.WorkloadSessionID,
			fmt.Errorf("verify active workload session: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}), true},
		{"PgBouncer login retry (08P01)", outagePeer(), outageBinding.WorkloadSessionID,
			fmt.Errorf("verify active workload session: %w", &pgconn.PgError{Severity: "FATAL", Code: "08P01", Message: "server login has been failing, cached error: the database system is shutting down (server_login_retry)"}), true},
		{"unknown, expired or revoked session", outagePeer(), outageBinding.WorkloadSessionID, ErrWorkloadUnauthorized, false},
		{"permanent database fault (missing table)", outagePeer(), outageBinding.WorkloadSessionID,
			fmt.Errorf("verify active workload session: %w", &pgconn.PgError{Severity: "ERROR", Code: "42P01", Message: "relation does not exist"}), false},
		{"wrong password to the database", outagePeer(), outageBinding.WorkloadSessionID,
			fmt.Errorf("verify active workload session: %w", &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"}), false},
		{"another workload's session", outagePeer(), "session-of-someone-else", nil, false},
		{"unverified peer", context.Background(), outageBinding.WorkloadSessionID, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &sessionVerifierStub{want: outageBinding, err: tc.verifierErr}
			authorizer, err := NewPeerAuthorizer(sessions)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := authorizer.AuthorizeWorkload(tc.ctx, tc.session, outageBinding.ProducerID)
			if identity != "" || err == nil {
				t.Fatalf("authorize = (%q, %v): a refusal must return no identity", identity, err)
			}
			if !errors.Is(err, ErrWorkloadUnauthorized) {
				t.Fatalf("authorize error %v is not ErrWorkloadUnauthorized: fail-closed callers would miss it", err)
			}
			if got := SessionStoreUnavailable(err); got != tc.wantMarked {
				t.Fatalf("SessionStoreUnavailable = %v, want %v", got, tc.wantMarked)
			}
			for _, leak := range []string{"session-7f3a", "producer-9c1d", "SQLSTATE", "terminating", "server login"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("refusal %q carries %q", err.Error(), leak)
				}
			}
		})
	}
}

func TestTheSessionStoreOutageIsLoggedOncePerOutageWithoutSessionDetails(t *testing.T) {
	down := fmt.Errorf("verify active workload session: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is in recovery mode"})
	sessions := &sessionVerifierStub{want: outageBinding, err: down}
	authorizer, err := NewPeerAuthorizer(sessions)
	if err != nil {
		t.Fatal(err)
	}
	logs := &recordedLog{}
	authorizer.WithLogger(slog.New(slog.NewJSONHandler(logs, nil)), "runtime output")

	for range 25 { // twenty-five requests during one outage
		_, _ = authorizer.AuthorizeOutput(outagePeer(), outageBinding.WorkloadSessionID, outageBinding.ProducerID)
	}
	sessions.err = nil
	for range 3 {
		if _, err := authorizer.AuthorizeOutput(outagePeer(), outageBinding.WorkloadSessionID, outageBinding.ProducerID); err != nil {
			t.Fatal(err)
		}
	}
	entries := logs.entries(t)
	if len(entries) != 2 || entries[0]["level"] != "WARN" || entries[1]["level"] != "INFO" {
		t.Fatalf("log = %v, want one WARN for the outage and one INFO for the recovery", entries)
	}
	if msg, _ := entries[0]["msg"].(string); !strings.HasPrefix(msg, "The runtime output server cannot read workload sessions;") {
		t.Fatalf("WARN = %q", msg)
	}
	if entries[0]["cause"] != "the database system is in recovery mode (SQLSTATE 57P03)" {
		t.Fatalf("WARN cause = %v", entries[0]["cause"])
	}
	if msg, _ := entries[1]["msg"].(string); !strings.HasPrefix(msg, "The runtime output server reads workload sessions again after ") {
		t.Fatalf("INFO = %q", msg)
	}
	for _, leak := range []string{"session-7f3a", "producer-9c1d", "worker.runtime.example"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("the outage log carries %q", leak)
		}
	}
}
