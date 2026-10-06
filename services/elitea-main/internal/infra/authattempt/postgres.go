// Package authattempt implements shared browser-authentication admission on
// PostgreSQL (elitea_auth.browser_attempt_windows, shared migration 0145).
package authattempt

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
)

const (
	maxAttempts       = int64(1_000_000)
	minWindow         = time.Second
	maxWindow         = browserapp.MaxBrowserAttemptRetryAfter
	minKeySecretBytes = 32
	maxKeySecretBytes = 64
	keyDomain         = "elitea-browser-attempt-v1\x00"

	// DefaultMaxConcurrentAdmissions bounds the database work of one replica.
	// Every credential attempt touches the shared global row, so a flood above
	// the limits must queue here, in memory, and not as lock waiters holding
	// connections of the service's one pool.
	DefaultMaxConcurrentAdmissions = 4
	// slotWait is how long an attempt waits for an admission slot. Past it
	// the attempt fails closed (503), it is not admitted.
	slotWait = time.Second
	// operationLimit bounds one admission's database work.
	operationLimit = 3 * time.Second
	// lockTimeout and statementTimeout bound the admit transaction. A timeout
	// fails closed.
	lockTimeout      = "500ms"
	statementTimeout = "2s"
	// maxDeniedEntries bounds the in-process denial cache. See deniedCache.
	maxDeniedEntries = 8192
	// windowOvershootSlack is how far past its policy a stored window's
	// remaining time may read and still be taken as the policy window. A
	// window is written as (writer's clock + policy) and read against the
	// reader's clock; the two are one database clock, but the read happens
	// at a slightly different instant. Anything longer than this is state
	// this limiter did not write, and fails closed (see deny).
	windowOvershootSlack = time.Second
)

var (
	ErrInvalidConfiguration = errors.New("invalid browser attempt limiter configuration")
	ErrUnavailable          = errors.New("browser attempt limiter unavailable")
)

// Policy is one fixed-window limit. The limiter snapshots every policy at
// construction; later mutation of Config cannot change a running instance.
type Policy struct {
	MaxAttempts int64
	Window      time.Duration
}

// Config names all supported stages explicitly so a newly added stage cannot
// silently inherit a weaker limit.
type Config struct {
	// KeySecret is stable deployment-wide keying material supplied by a secret
	// resolver. It prevents low-entropy client addresses and logins from being
	// recovered by enumerating the stored keys offline. Live rotation is
	// unsupported: every replica must change it only in a coordinated cutover.
	KeySecret []byte
	// Global is one deployment-wide row that every CREDENTIAL attempt counts
	// against. Its job is to cap the rate of password verification across all
	// clients and logins, which neither per-client nor per-login limits can
	// bound under a distributed guessing attack; refusing every credential
	// attempt while it is full is the intended fail-closed trade.
	//
	// A login BEGIN does not count against it. Begin verifies nothing; its
	// cost is one small pre-login session and transaction row, short-lived
	// and swept. Counting begins globally let anyone who can spread requests
	// over many client keys (an IPv6 /48 holds 65,536 /64s) stop every user
	// from even reaching the sign-in form. FormBegin's per-client limit
	// bounds begin on its own.
	Global               Policy
	FormBegin            Policy
	FormCredentialClient Policy
	FormCredentialLogin  Policy
	// MaxConcurrentAdmissions is the per-replica bound on admissions that
	// reach the database. Zero selects DefaultMaxConcurrentAdmissions.
	MaxConcurrentAdmissions int
}

type attemptKey [sha256.Size]byte

// PostgresAdmitter is a fixed window per key that starts at the key's first
// admitted attempt. It reproduces the Redis script it replaces exactly:
//
//   - one transaction reads every key of the attempt under a row lock;
//   - if ANY key's live window is at its limit, the attempt is denied with
//     Retry-After = the largest remaining window among those keys, and
//     NOTHING is written: a denied attempt neither counts nor extends a
//     window, and grows no row;
//   - otherwise every key is counted, and a key whose window has ended starts
//     a new window now.
//
// Three layers keep a flood from exhausting the service's pool. A key known to
// be at its limit is denied from memory (deniedCache). A lock-free read denies
// a key the database already holds at its limit. Only the rest take a slot
// (MaxConcurrentAdmissions) and the locked transaction, bounded by
// lock_timeout and statement_timeout. Every dependency failure and every
// timeout fails closed with ErrUnavailable.
type PostgresAdmitter struct {
	pool       *pgxpool.Pool
	keySecret  [sha256.Size]byte
	global     Policy
	formBegin  Policy
	formClient Policy
	formLogin  Policy
	slots      chan struct{}
	denied     *deniedCache
	now        func() time.Time
}

func NewPostgresAdmitter(pool *pgxpool.Pool, config Config) (*PostgresAdmitter, error) {
	if pool == nil || !validKeySecret(config.KeySecret) ||
		!validPolicy(config.Global) || !validPolicy(config.FormBegin) ||
		!validPolicy(config.FormCredentialClient) || !validPolicy(config.FormCredentialLogin) ||
		config.Global.Window < config.FormCredentialClient.Window ||
		config.Global.Window < config.FormCredentialLogin.Window ||
		config.MaxConcurrentAdmissions < 0 {
		return nil, ErrInvalidConfiguration
	}
	slots := config.MaxConcurrentAdmissions
	if slots == 0 {
		slots = DefaultMaxConcurrentAdmissions
	}
	return &PostgresAdmitter{
		pool:       pool,
		keySecret:  sha256.Sum256(config.KeySecret),
		global:     config.Global,
		formBegin:  config.FormBegin,
		formClient: config.FormCredentialClient,
		formLogin:  config.FormCredentialLogin,
		slots:      make(chan struct{}, slots),
		denied:     newDeniedCache(maxDeniedEntries),
		now:        time.Now,
	}, nil
}

// Admit admits up to MaxAttempts within the stage's fixed window. A
// dependency failure fails closed. Denied attempts do not extend the window
// or grow a queue or backlog.
func (a *PostgresAdmitter) Admit(ctx context.Context, attempt browserapp.BrowserAttempt) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if a == nil || a.pool == nil {
		return 0, ErrUnavailable
	}
	if err := attempt.Validate(); err != nil {
		return 0, err
	}
	policies, ok := a.policies(attempt.Stage)
	if !ok {
		return 0, browserapp.ErrInvalidBrowserAttempt
	}
	keys := a.keys(attempt)
	if len(keys) != len(policies) {
		return 0, ErrUnavailable
	}
	var largestWindow time.Duration
	for _, policy := range policies {
		largestWindow = max(largestWindow, policy.Window)
	}

	// Layer 1: a key known to be at its limit. Within a window a count only
	// rises, and only the window's end resets it, so the denial stays true.
	if retry, limited := a.denied.retryAfter(keys, a.now()); limited {
		return retry, browserapp.ErrAttemptLimited
	}

	select {
	case a.slots <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(slotWait):
		return 0, ErrUnavailable
	}
	defer func() { <-a.slots }()

	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	started := a.now()

	// Layer 2: the lock-free read. A stale read can only under-deny; the
	// locked path below checks again.
	windows, err := a.readWindows(opCtx, keys, policies)
	if err != nil {
		return 0, dependencyError(ctx)
	}
	if retry, limited := limitedBy(keys, policies, windows); limited {
		return a.deny(keys, policies, windows, retry, largestWindow, started)
	}

	// Layer 3: the locked transaction.
	retry, limited, windows, err := a.admitLocked(opCtx, keys, policies)
	if err != nil {
		return 0, dependencyError(ctx)
	}
	if limited {
		return a.deny(keys, policies, windows, retry, largestWindow, started)
	}
	return 0, nil
}

type storedWindow struct {
	attempts int64
	endsAt   time.Time
}

// window is one stored row as of the database clock: its count and the time
// left in it. remaining <= 0 means the window has ended and counts as zero.
type window struct {
	attempts  int64
	remaining time.Duration
}

// readWindows measures each window against clock_timestamp(), the instant
// the row is read, as the locked path does. now() is the transaction's START,
// which precedes the snapshot this statement reads: a window committed in
// between would read longer than its policy and turn a 429 into a 503.
func (a *PostgresAdmitter) readWindows(
	ctx context.Context,
	keys []attemptKey,
	policies []Policy,
) (map[attemptKey]window, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT key, attempts, (extract(epoch FROM window_ends_at - clock_timestamp()) * 1000000)::bigint
		FROM elitea_auth.browser_attempt_windows
		WHERE key = ANY($1)`, keyArguments(keys))
	if err != nil {
		return nil, err
	}
	windows, err := collectWindows(rows)
	if err != nil {
		return nil, err
	}
	clampWindows(keys, policies, windows)
	return windows, nil
}

// clampWindows takes a remaining time that reads at most
// windowOvershootSlack past its key's policy window as that window. A larger
// overshoot is left as read, so deny still fails it closed.
func clampWindows(keys []attemptKey, policies []Policy, windows map[attemptKey]window) {
	for index, key := range keys {
		current, found := windows[key]
		if !found {
			continue
		}
		limit := policies[index].Window
		if current.remaining > limit && current.remaining <= limit+windowOvershootSlack {
			current.remaining = limit
			windows[key] = current
		}
	}
}

func collectWindows(rows pgx.Rows) (map[attemptKey]window, error) {
	defer rows.Close()
	windows := make(map[attemptKey]window, 3)
	for rows.Next() {
		var (
			raw          []byte
			attempts     int64
			microseconds int64
		)
		if err := rows.Scan(&raw, &attempts, &microseconds); err != nil {
			return nil, err
		}
		if len(raw) != sha256.Size || attempts < 0 {
			return nil, ErrUnavailable
		}
		windows[attemptKey(raw)] = window{attempts: attempts, remaining: time.Duration(microseconds) * time.Microsecond}
	}
	return windows, rows.Err()
}

// admitLocked is the Redis script as one transaction. Each key's row is
// created if absent and locked in one statement, in key order, so two
// concurrent admissions always take their locks in the same order.
func (a *PostgresAdmitter) admitLocked(
	ctx context.Context,
	keys []attemptKey,
	policies []Policy,
) (retry time.Duration, limited bool, windows map[attemptKey]window, err error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return 0, false, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+lockTimeout+"'"); err != nil {
		return 0, false, nil, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
		return 0, false, nil, err
	}

	ordered := append([]attemptKey(nil), keys...)
	sort.Slice(ordered, func(left, right int) bool { return bytes.Compare(ordered[left][:], ordered[right][:]) < 0 })
	stored := make(map[attemptKey]storedWindow, len(keys))
	for _, key := range ordered {
		var row storedWindow
		// A new row starts as an ENDED window (attempts 0, ending now), so it
		// counts as zero. DO UPDATE with an unchanged value takes the row lock
		// on an existing row; DO NOTHING would not.
		if err := tx.QueryRow(ctx, `
			INSERT INTO elitea_auth.browser_attempt_windows AS current (key, attempts, window_ends_at)
			VALUES ($1, 0, now())
			ON CONFLICT (key) DO UPDATE SET attempts = current.attempts
			RETURNING attempts, window_ends_at`,
			key[:],
		).Scan(&row.attempts, &row.endsAt); err != nil {
			return 0, false, nil, err
		}
		if row.attempts < 0 {
			return 0, false, nil, ErrUnavailable
		}
		stored[key] = row
	}

	// The clock is read AFTER every lock is held. now() is the transaction's
	// START time, which can precede the commit of a window this transaction
	// waited on; measured against it, that window would look longer than its
	// policy. The Redis script read the server clock when it ran, which is
	// this instant.
	var reference time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&reference); err != nil {
		return 0, false, nil, err
	}
	windows = make(map[attemptKey]window, len(keys))
	for key, row := range stored {
		windows[key] = window{attempts: row.attempts, remaining: row.endsAt.Sub(reference)}
	}
	clampWindows(keys, policies, windows)

	if retry, limited := limitedBy(keys, policies, windows); limited {
		// Rollback (deferred) removes any row this attempt created.
		return retry, true, windows, nil
	}

	newCounts := make([]int64, len(keys))
	newEnds := make([]time.Time, len(keys))
	for index, key := range keys {
		if windows[key].remaining <= 0 {
			newCounts[index] = 1
			newEnds[index] = reference.Add(policies[index].Window)
			continue
		}
		// An active window keeps its stored end: only the count changes.
		newCounts[index] = stored[key].attempts + 1
		newEnds[index] = stored[key].endsAt
		if newCounts[index] > policies[index].MaxAttempts {
			return 0, false, nil, ErrUnavailable
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.browser_attempt_windows AS current
		SET attempts = next.attempts, window_ends_at = next.window_ends_at
		FROM unnest($1::bytea[], $2::bigint[], $3::timestamptz[]) AS next(key, attempts, window_ends_at)
		WHERE current.key = next.key`,
		keyArguments(keys), newCounts, newEnds,
	); err != nil {
		return 0, false, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, nil, err
	}
	committed = true
	return 0, false, windows, nil
}

// limitedBy reports whether any key's live window is at its limit, and the
// largest remaining window among those keys.
func limitedBy(keys []attemptKey, policies []Policy, windows map[attemptKey]window) (time.Duration, bool) {
	var retry time.Duration
	for index, key := range keys {
		current, found := windows[key]
		if !found || current.remaining <= 0 {
			continue
		}
		if current.attempts >= policies[index].MaxAttempts && current.remaining > retry {
			retry = current.remaining
		}
	}
	return retry, retry > 0
}

// deny records every key that is at its limit in the denial cache and
// answers ErrAttemptLimited. A Retry-After longer than any policy window is
// stored state this limiter did not write, so it fails closed instead.
func (a *PostgresAdmitter) deny(
	keys []attemptKey,
	policies []Policy,
	windows map[attemptKey]window,
	retry time.Duration,
	largestWindow time.Duration,
	started time.Time,
) (time.Duration, error) {
	if retry <= 0 || retry > largestWindow {
		return 0, ErrUnavailable
	}
	for index, key := range keys {
		current := windows[key]
		if current.remaining > 0 && current.attempts >= policies[index].MaxAttempts {
			// started precedes the database read, so this deadline is never
			// later than the window's real end.
			a.denied.add(key, started.Add(current.remaining))
		}
	}
	return retry, browserapp.ErrAttemptLimited
}

func (a *PostgresAdmitter) policies(stage browserapp.BrowserAttemptStage) ([]Policy, bool) {
	switch stage {
	case browserapp.BrowserAttemptFormBegin:
		// No global policy: see Config.Global.
		return []Policy{a.formBegin}, true
	case browserapp.BrowserAttemptFormCredential:
		return []Policy{a.global, a.formClient, a.formLogin}, true
	default:
		return nil, false
	}
}

func (a *PostgresAdmitter) keys(attempt browserapp.BrowserAttempt) []attemptKey {
	clientKey := a.key(attempt.Stage, "client", []byte(attempt.ClientKey))
	if attempt.Stage != browserapp.BrowserAttemptFormCredential {
		// A begin counts per client only; see Config.Global.
		return []attemptKey{clientKey}
	}
	globalKey := a.key("", "global", nil)
	// Enforce the credential policy independently per client and per login.
	// A combined client+login bucket can be bypassed by spraying many accounts
	// from one client or one account from many clients.
	return []attemptKey{
		globalKey,
		clientKey,
		a.key(attempt.Stage, "login", attempt.LoginDigest[:]),
	}
}

func (a *PostgresAdmitter) key(stage browserapp.BrowserAttemptStage, dimension string, value []byte) attemptKey {
	digest := hmac.New(sha256.New, a.keySecret[:])
	_, _ = digest.Write([]byte(keyDomain))
	_, _ = digest.Write([]byte(stage))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(dimension))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(value)
	var key attemptKey
	copy(key[:], digest.Sum(nil))
	return key
}

func keyArguments(keys []attemptKey) [][]byte {
	arguments := make([][]byte, len(keys))
	for index := range keys {
		arguments[index] = keys[index][:]
	}
	return arguments
}

func validPolicy(policy Policy) bool {
	return policy.MaxAttempts > 0 && policy.MaxAttempts <= maxAttempts &&
		policy.Window >= minWindow && policy.Window <= maxWindow &&
		policy.Window%time.Millisecond == 0
}

func validKeySecret(secret []byte) bool {
	return len(secret) >= minKeySecretBytes && len(secret) <= maxKeySecretBytes
}

// dependencyError keeps the caller's cancellation and deadline visible.
// Every other failure, including this limiter's own timeouts, is
// ErrUnavailable, with no dependency detail.
func dependencyError(ctx context.Context) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return ErrUnavailable
}

// deniedCache remembers, per replica, keys that are at their limit and the
// time their window ends. It is exact, not a second policy: a window's count
// never falls before the window ends, so a key at its limit stays denied
// until then on every replica. It only saves the database the work of saying
// so again. It is bounded; when it is full of live entries a new denial is
// not remembered, and the database answers instead.
type deniedCache struct {
	mu      sync.Mutex
	limit   int
	entries map[attemptKey]time.Time
}

func newDeniedCache(limit int) *deniedCache {
	return &deniedCache{limit: limit, entries: make(map[attemptKey]time.Time)}
}

func (c *deniedCache) retryAfter(keys []attemptKey, now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var retry time.Duration
	for _, key := range keys {
		deadline, found := c.entries[key]
		if !found {
			continue
		}
		if !deadline.After(now) {
			delete(c.entries, key)
			continue
		}
		retry = max(retry, deadline.Sub(now))
	}
	return retry, retry > 0
}

func (c *deniedCache) add(key attemptKey, deadline time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.entries[key]; !found && len(c.entries) >= c.limit {
		now := time.Now()
		for candidate, candidateDeadline := range c.entries {
			if !candidateDeadline.After(now) {
				delete(c.entries, candidate)
			}
		}
		if len(c.entries) >= c.limit {
			return
		}
	}
	c.entries[key] = deadline
}

var _ browserapp.AttemptAdmitter = (*PostgresAdmitter)(nil)
