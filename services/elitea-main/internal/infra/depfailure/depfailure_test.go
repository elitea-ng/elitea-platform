package depfailure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

func pgError(code, message string) error {
	return &pgconn.PgError{Severity: "FATAL", Code: code, Message: message}
}

const unreachableDSN = "postgres://elitea_user:s3cret-password@127.0.0.1:1/elitea_db?connect_timeout=2"

// connectFailure is a real pgx connect failure: a ConnectError whose text
// names the user, database, host and port around the refused dial.
func connectFailure(t *testing.T) error {
	t.Helper()
	_, err := pgconn.Connect(context.Background(), unreachableDSN)
	if err == nil {
		t.Fatal("connecting to a closed port succeeded")
	}
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		t.Fatalf("connect error = %T, want *pgconn.ConnectError", err)
	}
	return fmt.Errorf("begin transaction: %w", err)
}

// closedPoolFailure is what a pool returns after Close: it never reopens.
func closedPoolFailure(t *testing.T) error {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), unreachableDSN)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	_, err = pool.Acquire(context.Background())
	if err == nil {
		t.Fatal("a closed pool handed out a connection")
	}
	return fmt.Errorf("acquire: %w", err)
}

func TestTransientKeepsOnlyFailuresThatHealOnTheirOwn(t *testing.T) {
	alive := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithTimeout(context.Background(), 0)
	defer expire()
	<-expired.Done()

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	cases := []struct {
		name  string
		err   error
		owner context.Context
		want  bool
	}{
		// Kept: each heals without anything inside Main changing.
		{"PgBouncer cached login failure 08P01", pgError("08P01", "server login has been failing, cached error: the database system is shutting down (server_login_retry)"), alive, true},
		{"connection failure 08006", pgError("08006", "connection failure"), alive, true},
		{"admin shutdown 57P01", pgError("57P01", "terminating connection due to administrator command"), alive, true},
		{"crash shutdown 57P02", pgError("57P02", "terminating connection because of crash of another server process"), alive, true},
		{"recovery mode 57P03", pgError("57P03", "the database system is in recovery mode"), alive, true},
		{"too many connections 53300", pgError("53300", "sorry, too many clients already"), alive, true},
		{"recovery mode inside a wrapped connect error", fmt.Errorf("acquire: %w", fmt.Errorf("connect: %w", pgError("57P03", "the database system is starting up"))), alive, true},
		{"connection refused", fmt.Errorf("dial: %w", refused), alive, true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, alive, true},
		{"host not resolved while the container restarts", &net.DNSError{Err: "no such host", Name: "postgres", IsNotFound: true}, alive, true},
		{"broken pooled connection", fmt.Errorf("query: %w", pgconn.ErrConnClosed), alive, true},
		{"unexpected EOF", fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), alive, true},
		{"own timeout while the owner is alive", fmt.Errorf("sweep: %w", context.DeadlineExceeded), alive, true},
		{"NATS request timeout", fmt.Errorf("publish: %w", nats.ErrTimeout), alive, true},
		{"JetStream has no responder yet", fmt.Errorf("publish: %w", nats.ErrNoResponders), alive, true},
		{"NATS reconnect buffer full", nats.ErrReconnectBufExceeded, alive, true},

		// Removed or never members: none of these heals by waiting.
		{"closed pgx pool", closedPoolFailure(t), alive, false},
		{"refused connect error", connectFailure(t), alive, true},
		{"closed NATS connection", fmt.Errorf("publish: %w", nats.ErrConnectionClosed), alive, false},
		{"NATS gave up reconnecting", nats.ErrNoServers, alive, false},
		{"NATS drained while reconnecting", nats.ErrConnectionReconnecting, alive, false},
		{"wrong password inside a connect error", fmt.Errorf("connect: %w", pgError("28P01", "password authentication failed")), alive, false},
		{"database does not exist", pgError("3D000", "database \"x\" does not exist"), alive, false},
		{"PgBouncer: no such database", pgError("08P01", "no such database: elitea"), alive, false},
		{"PgBouncer: no such user", pgError("08P01", "no such user"), alive, false},
		{"PostgreSQL protocol violation", pgError("08P01", "invalid frontend message type 0"), alive, false},
		{"PgBouncer cannot reach the server", pgError("08P01", "pgbouncer cannot connect to server"), alive, true},
		{"serialization failure is the caller's to retry", pgError("40001", "could not serialize access"), alive, false},
		{"programming error", errors.New("runner invariant broken"), alive, false},
		{"own timeout after the owner's deadline", fmt.Errorf("sweep: %w", context.DeadlineExceeded), expired, false},
		{"own timeout after the owner was cancelled", fmt.Errorf("sweep: %w", context.DeadlineExceeded), cancelled, false},
		{"owner cancellation", context.Canceled, alive, false},
		{"nil", nil, alive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Transient(tc.err, tc.owner); got != tc.want {
				t.Fatalf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDescribeKeepsTheCauseAndDropsConnectionDetails(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"server answer", fmt.Errorf("begin: %w", pgError("08P01", "server login has been failing")), "server login has been failing (SQLSTATE 08P01)"},
		{"refused", fmt.Errorf("x: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), "the connection was refused"},
		{"reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "the connection was reset"},
		{"timeout", fmt.Errorf("sweep: %w", context.DeadlineExceeded), "no answer in time"},
		{"closed pool", closedPoolFailure(t), "the connection pool is closed"},
		{"NATS", nats.ErrNoResponders, nats.ErrNoResponders.Error()},
		{"connect error", connectFailure(t), "the connection was refused"},
		{"other", errors.New("boom"), "an unclassified error (*errors.errorString)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Describe(tc.err)
			if got != tc.want {
				t.Fatalf("Describe = %q, want %q", got, tc.want)
			}
			for _, secret := range []string{"elitea_user", "s3cret-password", "db.internal.example", "elitea_db", "5432"} {
				if strings.Contains(got, secret) {
					t.Fatalf("Describe = %q leaks %q", got, secret)
				}
			}
		})
	}
}
