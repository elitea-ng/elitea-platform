// Package depfailure decides whether a failure to reach PostgreSQL or NATS is
// one that heals on its own, and describes such a failure for an operator
// without the connection details.
//
// The classifier is closed and deliberately narrow (FU-275): it lists only
// conditions that end without anything inside the process changing. A closed
// pgx pool, a closed NATS connection, a wrong password or a missing database
// never heal by waiting, so they are not transient; a caller that restarts on
// them would loop for ever.
package depfailure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
)

// transientSQLStates are the SQLSTATEs a server returns while it is shutting
// down, starting, recovering or full. Class 08 (connection exception) is
// matched by prefix in Transient, except 08P01, below.
var transientSQLStates = map[string]struct{}{
	"57P01": {}, // admin_shutdown
	"57P02": {}, // crash_shutdown
	"57P03": {}, // cannot_connect_now: starting up or in recovery
	"53300": {}, // too_many_connections
}

// transientPgBouncerMessages are the 08P01 answers that heal on their own.
// PgBouncer reports almost every refusal as 08P01 (protocol_violation),
// including permanent ones ("no such database", "no such user", a failed
// password), and PostgreSQL uses 08P01 for real protocol violations, so 08P01
// is transient only with one of these messages.
var transientPgBouncerMessages = []string{
	"server login has been failing",
	"pgbouncer cannot connect to server",
	"server conn crashed",
	"query_wait_timeout",
	"the database system is shutting down",
	"the database system is starting up",
	"the database system is in recovery mode",
}

// transientNATSErrors are the errors a live NATS connection returns while it
// reconnects (MaxReconnects(-1)): a request finds no answer, JetStream has no
// responder yet, or the reconnect buffer is full. ErrConnectionClosed and
// ErrNoServers are absent on purpose: with unlimited reconnects they occur
// only after an explicit Close or when the connection gives up, which is
// permanent.
var transientNATSErrors = []error{
	nats.ErrTimeout,
	nats.ErrNoResponders,
	nats.ErrReconnectBufExceeded,
}

// Transient reports whether err is a dependency failure that heals on its
// own. owner is the context of the work that failed: a deadline is transient
// only while owner is still alive, because an owner's own cancellation or
// deadline is a stop, not an outage.
func Transient(err error, owner context.Context) bool {
	if err == nil {
		return false
	}
	ownerAlive := owner == nil || owner.Err() == nil
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// The server answered: its SQLSTATE decides, also when the answer
		// arrived inside a ConnectError (a wrong password is 28P01 there).
		if pgErr.Code == "08P01" {
			return hasTransientPgBouncerMessage(pgErr.Message)
		}
		return strings.HasPrefix(pgErr.Code, "08") || isTransientSQLState(pgErr.Code)
	}
	for _, natsErr := range transientNATSErrors {
		if errors.Is(err, natsErr) {
			return true
		}
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return ownerAlive
	}
	if errors.Is(err, pgconn.ErrConnClosed) || errors.Is(err, io.ErrUnexpectedEOF) {
		// pgx discards a broken connection and dials a fresh one next time.
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// A name that does not resolve now resolved at boot (every pool and
	// connection is dialled and pinged there, and a mistyped host fails the
	// boot), so at run time it is a container or pod being replaced.
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && (dnsErr.IsTemporary || dnsErr.IsNotFound || dnsErr.IsTimeout)
}

func hasTransientPgBouncerMessage(message string) bool {
	for _, transient := range transientPgBouncerMessages {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

func isTransientSQLState(code string) bool {
	_, ok := transientSQLStates[code]
	return ok
}

// Describe returns one short, readable cause for err. It never repeats a
// connection string, user, database name, host or SQL text: a server answer
// keeps its SQLSTATE and message, a network failure keeps only its kind.
func Describe(err error) string {
	if err == nil {
		return "no error"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("%s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
	}
	for _, natsErr := range transientNATSErrors {
		if errors.Is(err, natsErr) {
			return natsErr.Error()
		}
	}
	switch {
	case errors.Is(err, nats.ErrConnectionClosed):
		return nats.ErrConnectionClosed.Error()
	case errors.Is(err, nats.ErrNoServers):
		return nats.ErrNoServers.Error()
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "the connection was reset"
	case errors.Is(err, context.Canceled):
		return "the work was cancelled"
	case errors.Is(err, context.DeadlineExceeded), pgconn.Timeout(err):
		return "no answer in time"
	case errors.Is(err, pgconn.ErrConnClosed), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "the connection closed unexpectedly"
	case strings.Contains(err.Error(), "closed pool"):
		return "the connection pool is closed"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the host name did not resolve"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return "the network did not answer in time"
		}
		return "a network error (" + opErr.Op + ")"
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return "could not connect to the database"
	}
	return fmt.Sprintf("an unclassified error (%T)", err)
}

// Dependency names the outage a failure that heals on its own belongs to:
// "nats" for a NATS failure, "db" for everything else Transient accepts
// (PostgreSQL answers, broken or refused connections, timeouts), and "" for
// a failure that is not Transient. The names are the readiness monitor's
// Transitions keys, so a runner's failure is demoted only while that
// dependency's outage is being reported (FU-299 C3).
func Dependency(err error) string {
	if !Transient(err, nil) {
		return ""
	}
	for _, natsErr := range transientNATSErrors {
		if errors.Is(err, natsErr) {
			return "nats"
		}
	}
	return "db"
}
