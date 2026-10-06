package authattempt

import (
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authstatetest"
)

func TestNewPostgresAdmitterRejectsIncompleteConfiguration(t *testing.T) {
	valid := validConfig()
	pool := authstatetest.ClosedPool(t)
	if _, err := NewPostgresAdmitter(nil, valid); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil pool error = %v", err)
	}
	invalid := []Config{
		{},
		func() Config { config := valid; config.KeySecret = []byte("short"); return config }(),
		func() Config { config := valid; config.KeySecret = make([]byte, maxKeySecretBytes+1); return config }(),
		func() Config { config := valid; config.Global.MaxAttempts = 0; return config }(),
		func() Config { config := valid; config.Global.Window = 30 * time.Second; return config }(),
		func() Config { config := valid; config.FormBegin.MaxAttempts = 0; return config }(),
		func() Config {
			config := valid
			config.FormCredentialClient.MaxAttempts = maxAttempts + 1
			return config
		}(),
		func() Config { config := valid; config.FormCredentialLogin.Window = 0; return config }(),
		func() Config { config := valid; config.OIDCBegin.Window = time.Millisecond; return config }(),
		func() Config {
			config := valid
			config.OIDCCallback.Window = maxWindow + time.Millisecond
			return config
		}(),
		func() Config { config := valid; config.MaxConcurrentAdmissions = -1; return config }(),
	}
	for index, config := range invalid {
		if _, err := NewPostgresAdmitter(pool, config); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("invalid config %d error = %v", index, err)
		}
	}
}

func TestPostgresAdmitterValidatesBeforeTheDatabaseAndPreservesCancellation(t *testing.T) {
	limiter, err := NewPostgresAdmitter(authstatetest.ClosedPool(t), validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Admit(context.Background(), browserapp.BrowserAttempt{
		ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormCredential,
	}); !errors.Is(err, browserapp.ErrInvalidBrowserAttempt) {
		t.Fatalf("invalid attempt error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Admit(ctx, browserapp.BrowserAttempt{
		ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormBegin,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}
}

func TestPostgresAdmitterHashesStageClientAndLoginIntoSeparateKeys(t *testing.T) {
	limiter, err := NewPostgresAdmitter(authstatetest.ClosedPool(t), validConfig())
	if err != nil {
		t.Fatal(err)
	}
	digestA := sha256.Sum256([]byte("admin"))
	digestB := sha256.Sum256([]byte("viewer"))
	attempts := []browserapp.BrowserAttempt{
		{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormBegin},
		{ClientKey: "192.0.2.8", Stage: browserapp.BrowserAttemptFormBegin},
		{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptOIDCBegin},
		{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormCredential, LoginDigest: digestA},
		{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormCredential, LoginDigest: digestB},
	}
	keysByAttempt := make([][]attemptKey, 0, len(attempts))
	for _, attempt := range attempts {
		keys := limiter.keys(attempt)
		want := 2
		if attempt.Stage == browserapp.BrowserAttemptFormCredential {
			want = 3
		}
		if len(keys) != want {
			t.Fatalf("keys for %+v = %d, want %d", attempt, len(keys), want)
		}
		keysByAttempt = append(keysByAttempt, keys)
	}
	if keysByAttempt[0][0] != keysByAttempt[1][0] ||
		keysByAttempt[0][0] != keysByAttempt[2][0] ||
		keysByAttempt[0][1] == keysByAttempt[1][1] ||
		keysByAttempt[0][1] == keysByAttempt[2][1] ||
		keysByAttempt[3][1] != keysByAttempt[4][1] ||
		keysByAttempt[3][2] == keysByAttempt[4][2] ||
		keysByAttempt[3][1] == keysByAttempt[3][2] {
		t.Fatalf("unexpected key separation: %v", keysByAttempt)
	}
}

func TestPostgresAdmitterSnapshotsKeySecret(t *testing.T) {
	config := validConfig()
	limiter, err := NewPostgresAdmitter(authstatetest.ClosedPool(t), config)
	if err != nil {
		t.Fatal(err)
	}
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormBegin}
	want := limiter.keys(attempt)[1]
	for index := range config.KeySecret {
		config.KeySecret[index] = 0
	}
	if got := limiter.keys(attempt)[1]; got != want {
		t.Fatalf("key changed after caller mutated config secret")
	}
}

// Two replicas (two pools, two denial caches) race 64 attempts at a limit of
// 7. Exactly 7 are admitted, the stored count is exactly 7 (the 57 denials did
// not count), and only the two keys of the attempt exist.
func TestPostgresAdmitterIsAtomicAcrossInstancesAndDoesNotGrowDeniedBacklog(t *testing.T) {
	pool := authstatetest.Pool(t, 8)
	otherPool := secondPool(t, pool)
	config := validConfig()
	config.FormBegin = Policy{MaxAttempts: 7, Window: time.Minute}
	config.MaxConcurrentAdmissions = 6
	first := admitterOn(t, pool, config)
	second := admitterOn(t, otherPool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.7", Stage: browserapp.BrowserAttemptFormBegin}

	var admitted, limited, unexpected atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			limiter := first
			if index%2 == 1 {
				limiter = second
			}
			retry, err := limiter.Admit(context.Background(), attempt)
			switch {
			case err == nil && retry == 0:
				admitted.Add(1)
			case errors.Is(err, browserapp.ErrAttemptLimited) && retry > 0 && retry <= time.Minute:
				limited.Add(1)
			default:
				t.Errorf("attempt %d: retry=%s err=%v", index, retry, err)
				unexpected.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if admitted.Load() != config.FormBegin.MaxAttempts || limited.Load() != 64-config.FormBegin.MaxAttempts || unexpected.Load() != 0 {
		t.Fatalf("admitted=%d limited=%d unexpected=%d", admitted.Load(), limited.Load(), unexpected.Load())
	}
	if count := windowRows(t, pool); count != 2 {
		t.Fatalf("rows = %d, want the global and the client window", count)
	}
	keys := first.keys(attempt)
	if got := storedAttempts(t, pool, keys[1]); got != config.FormBegin.MaxAttempts {
		t.Fatalf("stored client count = %d, want %d: denials were counted", got, config.FormBegin.MaxAttempts)
	}

	// A distinct stage has an independent window, and the window's end
	// admits again.
	if _, err := second.Admit(context.Background(), browserapp.BrowserAttempt{
		ClientKey: attempt.ClientKey, Stage: browserapp.BrowserAttemptOIDCBegin,
	}); err != nil {
		t.Fatal(err)
	}
	endAllWindows(t, pool)
	fresh := admitterOn(t, pool, config) // a replica that has not cached the denial
	if _, err := fresh.Admit(context.Background(), attempt); err != nil {
		t.Fatalf("post-expiry admit: %v", err)
	}
	if got := storedAttempts(t, pool, keys[1]); got != 1 {
		t.Fatalf("count after the window ended = %d, want a new window at 1", got)
	}
}

// The window starts at the first admitted attempt and does not move: neither
// later admitted attempts nor denied ones extend it. Retry-After is the time
// left in that window.
func TestPostgresAdmitterFixedWindowStartsAtTheFirstAttemptAndDenialsDoNotExtendIt(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.FormBegin = Policy{MaxAttempts: 2, Window: 10 * time.Second}
	limiter := admitterOn(t, pool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.9", Stage: browserapp.BrowserAttemptFormBegin}
	clientKey := limiter.keys(attempt)[1]

	if _, err := limiter.Admit(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	end := windowEnd(t, pool, clientKey)
	if remaining := time.Until(end); remaining <= 9*time.Second || remaining > 10*time.Second {
		t.Fatalf("window after the first attempt = %s, want 10s from now", remaining)
	}
	if _, err := limiter.Admit(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if got := windowEnd(t, pool, clientKey); !got.Equal(end) {
		t.Fatalf("an admitted attempt moved the window end from %s to %s", end, got)
	}
	for range 5 {
		retry, err := limiter.Admit(context.Background(), attempt)
		if !errors.Is(err, browserapp.ErrAttemptLimited) || retry <= 0 || retry > 10*time.Second {
			t.Fatalf("over-limit attempt: retry=%s err=%v", retry, err)
		}
	}
	// A replica with an empty cache, so the database answers the denial.
	fresh := admitterOn(t, pool, config)
	retry, err := fresh.Admit(context.Background(), attempt)
	if !errors.Is(err, browserapp.ErrAttemptLimited) || retry <= 0 || retry > 10*time.Second {
		t.Fatalf("database denial: retry=%s err=%v", retry, err)
	}
	if got := windowEnd(t, pool, clientKey); !got.Equal(end) {
		t.Fatalf("denied attempts moved the window end from %s to %s", end, got)
	}
	if got := storedAttempts(t, pool, clientKey); got != 2 {
		t.Fatalf("stored count = %d, want 2", got)
	}
}

// Retry-After is the LARGEST remaining window among the keys at their limit.
func TestPostgresAdmitterRetryAfterIsTheLongestLimitedWindow(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	limiter := admitterOn(t, pool, validConfig())
	digest := sha256.Sum256([]byte("admin"))
	attempt := browserapp.BrowserAttempt{
		ClientKey: "192.0.2.10", Stage: browserapp.BrowserAttemptFormCredential, LoginDigest: digest,
	}
	keys := limiter.keys(attempt)
	setWindow(t, pool, keys[1], 5, 20*time.Second)  // client: at its limit of 5
	setWindow(t, pool, keys[2], 25, 40*time.Second) // login: at its limit of 25
	retry, err := limiter.Admit(context.Background(), attempt)
	if !errors.Is(err, browserapp.ErrAttemptLimited) || retry <= 39*time.Second || retry > 40*time.Second {
		t.Fatalf("retry=%s err=%v, want the login window's 40s", retry, err)
	}
	if got := storedAttempts(t, pool, keys[0]); got != 0 {
		t.Fatalf("the denied attempt counted on the global window: %d", got)
	}
}

func TestPostgresAdmitterLimitsFormCredentialsPerClientAndPerLogin(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.FormCredentialClient = Policy{MaxAttempts: 2, Window: time.Minute}
	config.FormCredentialLogin = Policy{MaxAttempts: 2, Window: time.Minute}
	limiter := admitterOn(t, pool, config)
	loginA := sha256.Sum256([]byte("admin"))
	loginB := sha256.Sum256([]byte("viewer"))
	attempt := func(clientKey string, loginDigest [sha256.Size]byte) error {
		_, admitErr := limiter.Admit(context.Background(), browserapp.BrowserAttempt{
			ClientKey: clientKey, Stage: browserapp.BrowserAttemptFormCredential, LoginDigest: loginDigest,
		})
		return admitErr
	}
	if err := attempt("192.0.2.7", loginA); err != nil {
		t.Fatal(err)
	}
	if err := attempt("192.0.2.7", loginA); err != nil {
		t.Fatal(err)
	}
	if err := attempt("192.0.2.8", loginA); !errors.Is(err, browserapp.ErrAttemptLimited) {
		t.Fatalf("distributed login spray error = %v", err)
	}
	if err := attempt("192.0.2.7", loginB); !errors.Is(err, browserapp.ErrAttemptLimited) {
		t.Fatalf("single-client account spray error = %v", err)
	}
	if err := attempt("192.0.2.8", loginB); err != nil {
		t.Fatalf("denied attempt mutated an unrelated window: %v", err)
	}
	if count := windowRows(t, pool); count != 5 {
		t.Fatalf("rows = %d, want one global and four dimensional windows", count)
	}
}

func TestPostgresAdmitterGlobalPolicyBoundsNovelKeyCardinality(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.Global = Policy{MaxAttempts: 3, Window: time.Minute}
	limiter := admitterOn(t, pool, config)
	for index := 0; index < 3; index++ {
		if _, err := limiter.Admit(context.Background(), browserapp.BrowserAttempt{
			ClientKey: string(rune('a' + index)), Stage: browserapp.BrowserAttemptFormBegin,
		}); err != nil {
			t.Fatalf("admit %d: %v", index, err)
		}
	}
	for _, replica := range []*PostgresAdmitter{limiter, admitterOn(t, pool, config)} {
		for index := 0; index < 25; index++ {
			retry, err := replica.Admit(context.Background(), browserapp.BrowserAttempt{
				ClientKey: "novel-" + strconv.Itoa(index), Stage: browserapp.BrowserAttemptFormBegin,
			})
			if !errors.Is(err, browserapp.ErrAttemptLimited) || retry <= 0 {
				t.Fatalf("novel attempt %d retry=%s error=%v", index, retry, err)
			}
		}
	}
	if count := windowRows(t, pool); count != 4 {
		t.Fatalf("rows = %d, want one global and three admitted client windows", count)
	}
}

// The locked path can deny too: a replica whose lock-free read saw the
// window below its limit must still deny when the locked read sees it full.
func TestPostgresAdmitterLockedPathRollsBackADenial(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.FormBegin = Policy{MaxAttempts: 1, Window: time.Minute}
	limiter := admitterOn(t, pool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.11", Stage: browserapp.BrowserAttemptFormBegin}
	keys := limiter.keys(attempt)
	setWindow(t, pool, keys[1], 1, 30*time.Second)

	retry, limited, windows, err := limiter.admitLocked(context.Background(), keys, []Policy{config.Global, config.FormBegin})
	if err != nil || !limited || retry <= 29*time.Second || len(windows) != 2 {
		t.Fatalf("locked denial: retry=%s limited=%v windows=%v err=%v", retry, limited, windows, err)
	}
	// The global row it created to lock was rolled back with the denial.
	if count := windowRows(t, pool); count != 1 {
		t.Fatalf("rows = %d, want only the client window", count)
	}
}

// A known denial is answered from memory: with the database gone, the
// limited key is still denied, and an unknown key fails closed.
func TestPostgresAdmitterAnswersAKnownDenialWithoutTheDatabase(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.FormBegin = Policy{MaxAttempts: 1, Window: time.Minute}
	limiter := admitterOn(t, pool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.12", Stage: browserapp.BrowserAttemptFormBegin}
	if _, err := limiter.Admit(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Admit(context.Background(), attempt); !errors.Is(err, browserapp.ErrAttemptLimited) {
		t.Fatalf("second attempt = %v", err)
	}

	limiter.pool = authstatetest.ClosedPool(t)
	retry, err := limiter.Admit(context.Background(), attempt)
	if !errors.Is(err, browserapp.ErrAttemptLimited) || retry <= 0 || retry > time.Minute {
		t.Fatalf("cached denial: retry=%s err=%v", retry, err)
	}
	if _, err := limiter.Admit(context.Background(), browserapp.BrowserAttempt{
		ClientKey: "192.0.2.13", Stage: browserapp.BrowserAttemptFormBegin,
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown key without the database = %v, want %v", err, ErrUnavailable)
	}
}

// The lock-free read denies a full window even while another transaction
// holds that window's row lock: a flood of denials queues no lock waiter.
func TestPostgresAdmitterDeniesAFullWindowWithoutWaitingOnItsLock(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.FormBegin = Policy{MaxAttempts: 1, Window: time.Minute}
	limiter := admitterOn(t, pool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.14", Stage: browserapp.BrowserAttemptFormBegin}
	keys := limiter.keys(attempt)
	setWindow(t, pool, keys[1], 1, 30*time.Second)
	holder := lockRow(t, pool, keys[1])
	defer func() { _ = holder.Rollback(context.Background()) }()

	started := time.Now()
	if _, err := limiter.Admit(context.Background(), attempt); !errors.Is(err, browserapp.ErrAttemptLimited) {
		t.Fatalf("denial under a held lock = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("the denial waited %s on the row lock", elapsed)
	}
}

func TestPostgresAdmitterFailsClosedOnLockTimeoutSaturationAndOutage(t *testing.T) {
	pool := authstatetest.Pool(t, 4)
	config := validConfig()
	config.MaxConcurrentAdmissions = 1
	limiter := admitterOn(t, pool, config)
	attempt := browserapp.BrowserAttempt{ClientKey: "192.0.2.15", Stage: browserapp.BrowserAttemptFormBegin}
	if _, err := limiter.Admit(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}

	t.Run("lock wait past lock_timeout", func(t *testing.T) {
		holder := lockRow(t, pool, limiter.keys(attempt)[0])
		defer func() { _ = holder.Rollback(context.Background()) }()
		started := time.Now()
		if _, err := limiter.Admit(context.Background(), attempt); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("admit under a held global lock = %v, want %v", err, ErrUnavailable)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("admission waited %s", elapsed)
		}
	})

	t.Run("no free admission slot", func(t *testing.T) {
		limiter.slots <- struct{}{}
		defer func() { <-limiter.slots }()
		if _, err := limiter.Admit(context.Background(), attempt); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("admit without a slot = %v, want %v", err, ErrUnavailable)
		}
	})

	t.Run("a stored window longer than any policy", func(t *testing.T) {
		other := browserapp.BrowserAttempt{ClientKey: "192.0.2.16", Stage: browserapp.BrowserAttemptFormBegin}
		setWindow(t, pool, limiter.keys(other)[1], 20, 2*time.Hour)
		if _, err := limiter.Admit(context.Background(), other); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("impossible window = %v, want %v", err, ErrUnavailable)
		}
	})

	t.Run("outage", func(t *testing.T) {
		down := admitterOn(t, authstatetest.ClosedPool(t), config)
		if _, err := down.Admit(context.Background(), attempt); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("outage error = %v", err)
		}
	})
}

func TestDeniedCacheIsBoundedAndForgetsEndedWindows(t *testing.T) {
	cache := newDeniedCache(2)
	now := time.Now()
	cache.add(attemptKey{1}, now.Add(time.Minute))
	cache.add(attemptKey{2}, now.Add(-time.Second))
	cache.add(attemptKey{3}, now.Add(time.Minute)) // evicts the ended entry for room
	cache.add(attemptKey{4}, now.Add(time.Minute)) // full of live entries: not remembered
	if _, limited := cache.retryAfter([]attemptKey{{4}}, now); limited {
		t.Fatal("a full cache remembered a new denial")
	}
	if retry, limited := cache.retryAfter([]attemptKey{{1}, {3}}, now); !limited || retry != time.Minute {
		t.Fatalf("live entries: retry=%s limited=%v", retry, limited)
	}
	if _, limited := cache.retryAfter([]attemptKey{{1}}, now.Add(time.Minute)); limited {
		t.Fatal("an ended window was still denied")
	}
}

func validConfig() Config {
	return Config{
		KeySecret:            []byte("0123456789abcdef0123456789abcdef"),
		Global:               Policy{MaxAttempts: 1000, Window: time.Minute},
		FormBegin:            Policy{MaxAttempts: 20, Window: time.Minute},
		FormCredentialClient: Policy{MaxAttempts: 5, Window: time.Minute},
		FormCredentialLogin:  Policy{MaxAttempts: 25, Window: time.Minute},
		OIDCBegin:            Policy{MaxAttempts: 20, Window: time.Minute},
		OIDCCallback:         Policy{MaxAttempts: 30, Window: time.Minute},
	}
}

func admitterOn(t *testing.T, pool *pgxpool.Pool, config Config) *PostgresAdmitter {
	t.Helper()
	limiter, err := NewPostgresAdmitter(pool, config)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

// secondPool opens another pool on the same isolated database: a second
// replica with its own connections.
func secondPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	other, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

func setWindow(t *testing.T, pool *pgxpool.Pool, key attemptKey, attempts int64, remaining time.Duration) {
	t.Helper()
	authstatetest.Exec(t, pool, `
		INSERT INTO elitea_auth.browser_attempt_windows (key, attempts, window_ends_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))
		ON CONFLICT (key) DO UPDATE SET attempts = EXCLUDED.attempts, window_ends_at = EXCLUDED.window_ends_at`,
		key[:], attempts, remaining.Seconds())
}

func endAllWindows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	authstatetest.Exec(t, pool,
		`UPDATE elitea_auth.browser_attempt_windows SET window_ends_at = now() - interval '1 millisecond'`)
}

func lockRow(t *testing.T, pool *pgxpool.Pool, key attemptKey) interface {
	Rollback(context.Context) error
} {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(),
		`SELECT 1 FROM elitea_auth.browser_attempt_windows WHERE key = $1 FOR UPDATE`, key[:]); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	return tx
}

func windowRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.browser_attempt_windows`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func storedAttempts(t *testing.T, pool *pgxpool.Pool, key attemptKey) int64 {
	t.Helper()
	var attempts int64
	err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM elitea_auth.browser_attempt_windows WHERE key = $1`, key[:]).Scan(&attempts)
	if err != nil {
		return 0
	}
	return attempts
}

func windowEnd(t *testing.T, pool *pgxpool.Pool, key attemptKey) time.Time {
	t.Helper()
	var end time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT window_ends_at FROM elitea_auth.browser_attempt_windows WHERE key = $1`, key[:]).Scan(&end); err != nil {
		t.Fatal(err)
	}
	return end
}
