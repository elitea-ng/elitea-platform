// Package nats is the gateway's hardened NATS JetStream client.
//
// It backs the budget-enforcement path (design §8): the authoritative budget
// counters live in a JetStream *counter stream* incremented atomically with the
// Nats-Incr header, the 80% soft-alert cooldown lives in a KV bucket, and the
// write-behind deltas are published to a limits stream.
//
// Hardening (design §8.5, a Build prerequisite): the connection sets
// Timeout=1s, and EVERY counter / KV operation is wrapped in
// context.WithTimeout(ctx, OpTimeout=150ms) so a NATS network partition trips
// the circuit breaker instead of hanging the /llm request. A lightweight
// circuit breaker (sony/gobreaker) wraps the counter+KV operations; callers use
// its state to drive the tiered-hybrid fail-mode FSM (§8.5).
//
// Nats-Incr reality (see the "nats-incr-is-stream-counter" note): NATS 2.12's
// atomic increment is a stream feature (StreamConfig.AllowMsgCounter), NOT a KV
// method — nats.go v1.52.0 exposes no KeyValue.Incr(). The counter is therefore
// a dedicated stream (GATEWAY_BUDGET) whose per-subject running total is
// returned in PubAck.Value on publish and read back via GetLastMsgForSubject.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
)

const (
	// ConnectTimeout bounds the initial dial and every request/reply the client
	// makes; a partition fails fast rather than hanging (design §8.5).
	ConnectTimeout = 1 * time.Second

	// OpTimeout bounds every individual counter / KV operation. Without it a
	// NATS partition hangs the request instead of tripping the breaker (§8.5).
	OpTimeout = 150 * time.Millisecond

	// RecoveryDedupeWindow is the budget counter stream's duplicate window. It
	// bounds how long a reused Nats-Msg-Id suppresses a re-applied recovery
	// increment (§8.5 step 2: "tag each row's contribution with an event_id
	// reused across retries … see the §8.6 dedup table"). It is set ≥ the
	// FORCED_CLOSED ceiling (LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN, default
	// 10m) so a crash-and-retry within a single outage cycle is always deduped
	// rather than double-counted onto the recovered counter.
	RecoveryDedupeWindow = 12 * time.Minute

	// IncrHeader is the NATS 2.12 atomic-increment header. nats.go v1.52.0 does
	// not define it (the KeyValue interface has no Incr), so we set it directly
	// on the published message; the counter stream (AllowMsgCounter) applies it
	// and returns the new running total in PubAck.Value.
	IncrHeader = "Nats-Incr"

	// BudgetStream is the counter stream holding int64 nano-USD budget counters,
	// one running total per subject gateway.budget.<scope>.<id>.<period>.
	BudgetStream = "GATEWAY_BUDGET"
	// budgetSubjectPrefix is the wildcard the counter stream binds.
	budgetSubjectRoot = "gateway.budget.counter"

	// AlertCooldownBucket is the KV bucket enforcing the 80% soft-alert cooldown
	// via kv.Create (SETNX-equivalent) + bucket TTL (design §8.3).
	AlertCooldownBucket = "GATEWAY_ALERT_COOLDOWN"

	// DeltasStream is the write-behind stream (§8.6); the scheduler drains it.
	DeltasStream = "GATEWAY_BUDGET_DELTAS"
	// DeltaSubject is the write-behind delta subject (§8.6).
	DeltaSubject = "gateway.budget.delta"
)

// ErrUnavailable is returned when the circuit breaker is open or a NATS
// operation exceeds OpTimeout. Callers (the governance store) map it onto the
// tiered-hybrid fail-mode FSM (§8.5) — it is an infrastructure signal, never a
// budget-policy decision.
var ErrUnavailable = errors.New("nats: unavailable")

// Config is the resolved NATS wiring for the gateway.
type Config struct {
	// URL is the NATS server URL. Empty disables NATS wiring (the gateway
	// then runs without budget enforcement — dev/test only). With client
	// material it must be tls:// and carry no credential.
	URL string
	// Name identifies this client in NATS monitoring.
	Name string
	// CBFailureThreshold trips the breaker after this many consecutive failures
	// (design §8.5, LLM_BUDGET_CB_FAILURE_THRESHOLD, default 3).
	CBFailureThreshold uint32
	// CBOpenDuration is how long the breaker stays open before probing half-open
	// (design §8.5, LLM_BUDGET_CB_OPEN_DURATION_SEC, default 10s).
	CBOpenDuration time.Duration
	// TLSCAFile, TLSCertFile and TLSKeyFile are the gateway's NATS client
	// identity (GATEWAY_NATS_TLS_*; #1076). All three or none.
	TLSCAFile   string
	TLSCertFile string
	TLSKeyFile  string
}

// withDefaults fills zero values with the design §8.5 defaults.
func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "elitea-llm-gateway"
	}
	if c.CBFailureThreshold == 0 {
		c.CBFailureThreshold = 3
	}
	if c.CBOpenDuration <= 0 {
		c.CBOpenDuration = 10 * time.Second
	}
	return c
}

// material reads the TLS settings through natsconn, which names the
// GATEWAY_NATS_TLS_* variables in its errors.
func (c Config) material() (natsconn.Material, error) {
	names := natsconn.EnvNames(envPrefix)
	vals := map[string]string{names[0]: c.TLSCAFile, names[1]: c.TLSCertFile, names[2]: c.TLSKeyFile}
	return natsconn.FromEnv(envPrefix, func(k string) (string, bool) {
		v, ok := vals[k]
		return v, ok
	})
}

// envPrefix is the gateway's NATS environment prefix (GATEWAY_NATS_URL,
// GATEWAY_NATS_TLS_*).
const envPrefix = "GATEWAY"

// conn is the minimal surface of *nats.Conn the client uses; it lets tests
// substitute a fake without a live server.
type conn interface {
	Close()
	IsClosed() bool
	// Publish + FlushTimeout are the core-NATS (non-JetStream) publish surface
	// used for platform events on gateway.events.* — subscribers (elitea-main
	// natsbus) use plain core subscriptions, so events must not be JetStream-
	// persisted under a stream that would shadow the subject space.
	Publish(subj string, data []byte) error
	FlushTimeout(d time.Duration) error
}

// The following narrow interfaces are the exact operation surface the
// breaker-wrapped methods call through. The real nats.go JetStream/Stream/KV
// types satisfy them, and tests inject fakes so the hardening logic (timeout,
// breaker, error mapping, counter parsing) is verifiable without a live server.

// publisher is the JetStream publish surface (IncrBudget, PublishDelta).
type publisher interface {
	PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// counterReader is the last-message-for-subject read surface (ReadBudget).
type counterReader interface {
	GetLastMsgForSubject(ctx context.Context, subject string) (*jetstream.RawStreamMsg, error)
}

// kvCreator is the SETNX-equivalent surface (TryAlertCooldown).
type kvCreator interface {
	Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error)
}

// assetBinder is the JetStream surface bindAssets needs: look a stream or a
// bucket up, never create one. The real jetstream.JetStream satisfies it;
// tests inject a fake so the verification is checkable without a server.
type assetBinder interface {
	Stream(ctx context.Context, stream string) (jetstream.Stream, error)
	KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error)
}

// Client is the hardened JetStream client. It is safe for concurrent use.
type Client struct {
	cfg    Config
	nc     conn
	js     jetstream.JetStream
	pub    publisher
	budget counterReader
	// ratelimit is the GATEWAY_RATELIMIT counter stream (ratelimit.go). It is a
	// separate handle from budget because the two streams have opposite
	// retention needs; see that file's header.
	ratelimit counterReader
	cooldown  kvCreator
	breaker   *gobreaker.CircuitBreaker[uint64]

	// onStateChange, if set, is invoked on every breaker transition — the
	// recovery-reconciliation goroutine (§8.5) subscribes here.
	// mu guards onStateChange against concurrent writes from OnBreakerStateChange
	// and reads from the gobreaker callback goroutine.
	mu            sync.RWMutex
	onStateChange func(from, to gobreaker.State)
}

// Connect dials NATS with the hardened timeout and binds to the budget
// counter stream, the rate-limit counter stream, the write-behind deltas
// stream and the cooldown KV bucket. It creates none of them: the
// nats-bootstrap Job owns every asset (#1076), and the permission table does
// not let the gateway's identity create or update a stream. A missing or
// misconfigured asset fails here with the asset and the fix named.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	if cfg.URL == "" {
		return nil, fmt.Errorf("nats: empty URL")
	}
	material, err := cfg.material()
	if err != nil {
		return nil, err
	}
	if err := material.CheckURL(cfg.URL); err != nil {
		return nil, err
	}
	if err := material.Check(); err != nil {
		return nil, err
	}
	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.Timeout(ConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500 * time.Millisecond),
		// The permission table lets this identity subscribe to its own
		// inbox prefix only.
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityGateway)),
	}
	if material.Enabled() {
		opts = append(opts,
			nats.Secure(natsconn.BaseTLSConfig()),
			// Both callbacks re-read their files on every handshake, so a
			// renewed certificate is presented on the next reconnect.
			nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs),
		)
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats: jetstream: %w", err)
	}
	c := &Client{cfg: cfg, nc: nc, js: js, pub: js}
	c.breaker = newBreaker(cfg, func(from, to gobreaker.State) {
		c.mu.RLock()
		fn := c.onStateChange
		c.mu.RUnlock()
		if fn != nil {
			fn(from, to)
		}
	})
	if err := c.bindAssets(ctx, js); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// newBreaker builds the KV/counter circuit breaker (design §8.5). It trips after
// CBFailureThreshold consecutive failures within a 5s window and probes
// half-open after CBOpenDuration.
//
// Fix #5 (nats-atomicity): context.Canceled signals that the *caller* gave up
// (browser stop-button, client timeout) — it is not evidence of NATS being
// unhealthy. Without IsExcluded, a burst of 3 concurrent client cancellations
// within the 5s Interval window opens the breaker and triggers degraded mode
// even when NATS is fully operational. The hook returns true to exclude
// context.Canceled from the failure count.
func newBreaker(cfg Config, onChange func(from, to gobreaker.State)) *gobreaker.CircuitBreaker[uint64] {
	threshold := cfg.CBFailureThreshold
	return gobreaker.NewCircuitBreaker[uint64](gobreaker.Settings{
		Name:        "nats-budget",
		MaxRequests: 1, // half-open probes one request at a time
		Interval:    5 * time.Second,
		Timeout:     cfg.CBOpenDuration,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= threshold
		},
		IsSuccessful: func(err error) bool {
			// context.Canceled means the caller gave up; do not count it as a
			// breaker success either — it carries no signal about NATS health.
			return err == nil
		},
		IsExcluded: func(err error) bool {
			// Exclude client-side cancellation from the breaker failure count.
			return errors.Is(err, context.Canceled)
		},
		OnStateChange: func(_ string, from, to gobreaker.State) {
			if onChange != nil {
				onChange(from, to)
			}
		},
	})
}

// OnBreakerStateChange registers a callback invoked on every breaker transition.
// The recovery-reconciliation goroutine (§8.5) uses this to fire on the
// open→half-open→closed edge. It is safe to call concurrently with ongoing
// operations; the callback is swapped under a write-lock.
func (c *Client) OnBreakerStateChange(fn func(from, to gobreaker.State)) {
	c.mu.Lock()
	c.onStateChange = fn
	c.mu.Unlock()
}

// BreakerState reports the current circuit-breaker state (design §8.5 FSM input).
func (c *Client) BreakerState() gobreaker.State { return c.breaker.State() }

// errAssetMissing is wrapped by every "asset does not exist" bind error.
var errAssetMissing = errors.New("created by the nats-bootstrap Job (deploy/helm/nats-bootstrap; compose: the nats-bootstrap service) — run it, then restart the gateway")

// bindAssets looks up every asset the gateway uses and verifies the settings
// its code depends on. It never creates or updates one (#1076): before, the
// gateway's CreateOrUpdate overwrote the bootstrap's retention on every boot.
// Only the properties the gateway's correctness rests on are checked; the
// rest (retention, replicas) is the bootstrap's to decide.
func (c *Client) bindAssets(ctx context.Context, b assetBinder) error {
	sctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	// Budget counter stream: Nats-Incr needs AllowMsgCounter, and the
	// recovery replay's reused Nats-Msg-Id needs the duplicate window to
	// cover RecoveryDedupeWindow (§8.5 step 2) or a retried replay is
	// counted twice.
	budget, err := bindStream(sctx, b, BudgetStream, func(cfg jetstream.StreamConfig) error {
		if !cfg.AllowMsgCounter {
			return errors.New("AllowMsgCounter is off, so Nats-Incr cannot increment it")
		}
		if !hasSubject(cfg.Subjects, budgetSubjectRoot+".>") {
			return fmt.Errorf("subjects %v do not include %s.>", cfg.Subjects, budgetSubjectRoot)
		}
		if cfg.Duplicates < RecoveryDedupeWindow {
			return fmt.Errorf("duplicate window %v is shorter than the %v recovery replay window, so a retried replay would be counted twice", cfg.Duplicates, RecoveryDedupeWindow)
		}
		return requireDirect(cfg)
	})
	if err != nil {
		return err
	}
	c.budget = budget

	// Rate-limit counter stream (ratelimit.go).
	rl, err := bindStream(sctx, b, RateLimitStream, func(cfg jetstream.StreamConfig) error {
		if !cfg.AllowMsgCounter {
			return errors.New("AllowMsgCounter is off, so Nats-Incr cannot increment it")
		}
		if !hasSubject(cfg.Subjects, rateLimitSubjectRoot+".>") {
			return fmt.Errorf("subjects %v do not include %s.>", cfg.Subjects, rateLimitSubjectRoot)
		}
		if cfg.MaxAge <= 0 {
			return errors.New("MaxAge is unset, so one subject per minute per scope grows without bound")
		}
		if cfg.MaxAge <= RateLimitWindow {
			return fmt.Errorf("MaxAge %v does not outlive one %v window, so a counter could expire while its window is open", cfg.MaxAge, RateLimitWindow)
		}
		return requireDirect(cfg)
	})
	if err != nil {
		return err
	}
	c.ratelimit = rl

	// Write-behind deltas stream (§8.6): PublishDelta's Nats-Msg-Id dedup
	// needs a duplicate window.
	if _, err := bindStream(sctx, b, DeltasStream, func(cfg jetstream.StreamConfig) error {
		if !hasSubject(cfg.Subjects, DeltaSubject) {
			return fmt.Errorf("subjects %v do not include %s", cfg.Subjects, DeltaSubject)
		}
		if cfg.Duplicates <= 0 {
			return errors.New("no duplicate window, so a re-published delta is applied twice")
		}
		return nil
	}); err != nil {
		return err
	}

	// Alert-cooldown KV: kv.Create is SETNX only while the key lives, so the
	// bucket needs a TTL or an alert fires once per scope and period forever.
	kv, err := b.KeyValue(sctx, AlertCooldownBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return fmt.Errorf("nats: KV bucket %s does not exist; it is %w", AlertCooldownBucket, errAssetMissing)
		}
		return fmt.Errorf("nats: bind KV bucket %s: %w", AlertCooldownBucket, err)
	}
	status, err := kv.Status(sctx)
	if err != nil {
		return fmt.Errorf("nats: read KV bucket %s: %w", AlertCooldownBucket, err)
	}
	if status.TTL() <= 0 {
		return fmt.Errorf("nats: KV bucket %s has no TTL, so a soft alert could never re-fire; re-run the nats-bootstrap Job", AlertCooldownBucket)
	}
	c.cooldown = kv
	return nil
}

// bindStream looks a stream up and runs verify on its configuration.
func bindStream(ctx context.Context, b assetBinder, name string, verify func(jetstream.StreamConfig) error) (jetstream.Stream, error) {
	st, err := b.Stream(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, fmt.Errorf("nats: stream %s does not exist; it is %w", name, errAssetMissing)
		}
		return nil, fmt.Errorf("nats: bind stream %s: %w", name, err)
	}
	info := st.CachedInfo()
	if info == nil {
		return nil, fmt.Errorf("nats: bind stream %s: no stream info", name)
	}
	if err := verify(info.Config); err != nil {
		return nil, fmt.Errorf("nats: stream %s: %w; it is configured by the nats-bootstrap Job — re-run it", name, err)
	}
	return st, nil
}

// requireDirect: the gateway reads a counter with GetLastMsgForSubject, which
// nats.go serves with a direct get on an allow_direct stream. The NATS
// permission table grants the gateway $JS.API.DIRECT.GET on its counter
// streams and not the stream API's MSG.GET, so a stream without allow_direct
// would fail every read with a permissions violation.
func requireDirect(cfg jetstream.StreamConfig) error {
	if !cfg.AllowDirect {
		return errors.New("allow_direct is off, so the gateway's reads (direct get) are not the ones its NATS permissions grant")
	}
	return nil
}

func hasSubject(subjects []string, want string) bool {
	for _, s := range subjects {
		if s == want {
			return true
		}
	}
	return false
}

// BudgetSubject builds the counter subject for a budget scope/period. The
// components are sanitised to the NATS token charset by the caller's own
// identifiers (scope ∈ {project,team,customer,global}; scope_id numeric/uuid;
// period unix seconds), so no token contains a dot or space.
func BudgetSubject(scope, scopeID string, periodStartUnix int64) string {
	return fmt.Sprintf("%s.%s.%s.%d", budgetSubjectRoot, scope, scopeID, periodStartUnix)
}

// IncrBudget atomically adds deltaNano (int64 nano-USD, may be negative for a
// correction) to the counter for subject and returns the new running total.
// The whole operation is breaker-guarded and OpTimeout-bounded (design §8.5).
//
// The breaker generic is uint64, so the int64 total is passed out through a
// closure variable to avoid a negative-overshoot total being corrupted by a
// uint64 round-trip (a negative int64 cast to uint64 becomes a huge value).
func (c *Client) IncrBudget(ctx context.Context, subject string, deltaNano int64) (int64, error) {
	var total int64
	_, err := c.breaker.Execute(func() (uint64, error) {
		octx, cancel := context.WithTimeout(ctx, OpTimeout)
		defer cancel()
		msg := &nats.Msg{
			Subject: subject,
			Header:  nats.Header{IncrHeader: []string{strconv.FormatInt(deltaNano, 10)}},
		}
		ack, err := c.pub.PublishMsg(octx, msg)
		if err != nil {
			return 0, err
		}
		t, perr := strconv.ParseInt(ack.Value, 10, 64)
		if perr != nil {
			// A non-counter stream (or an empty val) is a config error, not a
			// transient one — surface it without tripping on parse noise.
			return 0, fmt.Errorf("nats: counter ack %q: %w", ack.Value, perr)
		}
		total = t
		return 0, nil
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return total, nil
}

// IncrBudgetIdempotent atomically adds deltaNano to the counter for subject with
// a reused Nats-Msg-Id (eventID) so a retry after a crash between the increment
// and the recovery commit is deduped inside the stream's duplicate window rather
// than double-applied (design §8.5 step 2, §8.6 dedup). It returns the new
// running total and whether this call actually applied the increment (false ⇒ a
// duplicate was suppressed, so the contribution was already counted).
//
// It is breaker-guarded and OpTimeout-bounded like IncrBudget. On a suppressed
// duplicate the counter total may not be echoed in the ack, so the caller MUST
// treat applied=false as "already reconciled" and not re-derive the total from
// this call.
func (c *Client) IncrBudgetIdempotent(ctx context.Context, subject, eventID string, deltaNano int64) (total int64, applied bool, err error) {
	type result struct {
		total   int64
		applied bool
	}
	// The breaker generic is uint64; smuggle the two-field result through a
	// closure variable so the breaker still counts failures on this path.
	var out result
	_, berr := c.breaker.Execute(func() (uint64, error) {
		octx, cancel := context.WithTimeout(ctx, OpTimeout)
		defer cancel()
		msg := &nats.Msg{
			Subject: subject,
			Header: nats.Header{
				IncrHeader:            []string{strconv.FormatInt(deltaNano, 10)},
				jetstream.MsgIDHeader: []string{eventID},
			},
		}
		ack, perr := c.pub.PublishMsg(octx, msg)
		if perr != nil {
			return 0, perr
		}
		if ack.Duplicate {
			// Already applied within the duplicate window; do not re-count.
			out = result{applied: false}
			return 0, nil
		}
		t, cerr := strconv.ParseInt(ack.Value, 10, 64)
		if cerr != nil {
			return 0, fmt.Errorf("nats: counter ack %q: %w", ack.Value, cerr)
		}
		out = result{total: t, applied: true}
		return 0, nil
	})
	if berr != nil {
		return 0, false, mapErr(berr)
	}
	return out.total, out.applied, nil
}

// ReadBudget returns the current running total (int64 nano-USD) for subject, or
// 0 if the counter has never been incremented this period. Breaker-guarded and
// OpTimeout-bounded.
//
// The breaker generic is uint64, so the int64 total is passed out through a
// closure variable; we never cast a potentially-negative total through uint64
// (a negative total from a correction-overshoot would become a huge wrong value).
func (c *Client) ReadBudget(ctx context.Context, subject string) (int64, error) {
	var total int64
	_, err := c.breaker.Execute(func() (uint64, error) {
		octx, cancel := context.WithTimeout(ctx, OpTimeout)
		defer cancel()
		raw, err := c.budget.GetLastMsgForSubject(octx, subject)
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				total = 0
				return 0, nil // never incremented this period → 0
			}
			return 0, err
		}
		t, perr := counterValue(raw)
		if perr != nil {
			return 0, perr
		}
		total = t
		return 0, nil
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return total, nil
}

// counterValue extracts the running total from a counter-stream message.
//
// NATS 2.12 counter streams store the running total as a JSON body
// {"val":"<decimal>"} (verified against a live nats:2.12 server — the PubAck
// carries the same value already unwrapped into PubAck.Value by nats.go). A
// bare decimal payload is also accepted for forward/backward tolerance; the
// original implementation expected ONLY the bare form, so every live
// ReadBudget failed to parse and the budget path silently degraded to the
// Postgres fallback tier (found by the BFF.9x live gate run).
func counterValue(raw *jetstream.RawStreamMsg) (int64, error) {
	if raw == nil || len(raw.Data) == 0 {
		return 0, nil
	}
	if total, err := strconv.ParseInt(string(raw.Data), 10, 64); err == nil {
		return total, nil
	}
	var body struct {
		Val string `json:"val"`
	}
	if err := json.Unmarshal(raw.Data, &body); err == nil && body.Val != "" {
		total, perr := strconv.ParseInt(body.Val, 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("nats: counter payload %q: val is not an integer: %w", raw.Data, perr)
		}
		return total, nil
	}
	return 0, fmt.Errorf("nats: counter payload %q: neither bare integer nor {\"val\":\"N\"}", raw.Data)
}

// TryAlertCooldown attempts to claim the soft-alert cooldown for key. It returns
// true if the alert should fire (the key was freshly created) and false if a
// cooldown is already active (kv.Create failed with ErrKeyExists). The bucket's
// TTL expires the key so the next crossing after the window re-fires (§8.3).
func (c *Client) TryAlertCooldown(ctx context.Context, key string) (bool, error) {
	v, err := c.breaker.Execute(func() (uint64, error) {
		octx, cancel := context.WithTimeout(ctx, OpTimeout)
		defer cancel()
		if _, err := c.cooldown.Create(octx, key, []byte("1")); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				return 0, nil // cooldown active → suppress
			}
			return 0, err
		}
		return 1, nil // freshly claimed → fire
	})
	if err != nil {
		return false, mapErr(err)
	}
	return v == 1, nil
}

// PublishDelta publishes a write-behind delta to GATEWAY_BUDGET_DELTAS with
// Nats-Msg-Id=eventID for publish-side dedup within the stream's duplicate
// window (design §8.6). It is breaker-guarded and OpTimeout-bounded.
func (c *Client) PublishDelta(ctx context.Context, eventID string, payload []byte) error {
	_, err := c.breaker.Execute(func() (uint64, error) {
		octx, cancel := context.WithTimeout(ctx, OpTimeout)
		defer cancel()
		msg := &nats.Msg{
			Subject: DeltaSubject,
			Data:    payload,
			Header:  nats.Header{jetstream.MsgIDHeader: []string{eventID}},
		}
		if _, err := c.pub.PublishMsg(octx, msg); err != nil {
			return 0, err
		}
		return 0, nil
	})
	if err != nil {
		return mapErr(err)
	}
	return nil
}

// EventSubjectRoot is the reserved platform-event subject prefix. It mirrors
// elitea-main natsbus.SubjectRoot: channel "project:<id>:events" maps to
// "gateway.events.project.<id>.events".
const EventSubjectRoot = "gateway.events"

// eventSubjectForProject builds the per-project event subject the elitea-main
// natsbus EventBus subscribes to.
func eventSubjectForProject(projectID string) string {
	return EventSubjectRoot + ".project." + projectID + ".events"
}

// PublishSoftAlertEvent publishes a pre-marshalled event envelope onto the
// project's gateway.events.* subject via core NATS (not JetStream — the
// subscribers use plain subscriptions). The flush is bounded by the ctx
// deadline (capped at OpTimeout) so a wedged connection cannot stall the
// billing goroutine. Satisfies llmproxy.AlertEventPublisher.
func (c *Client) PublishSoftAlertEvent(ctx context.Context, projectID string, event []byte) error {
	// Deadline is evaluated BEFORE the publish: core NATS Publish is async and
	// buffered, so publishing first and then bailing out on an expired ctx would
	// still enqueue the event for the next flush while telling the caller the
	// publish failed. Check first so the returned error is truthful.
	timeout := OpTimeout
	if dl, ok := ctx.Deadline(); ok {
		if until := time.Until(dl); until < timeout {
			timeout = until
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	if err := c.nc.Publish(eventSubjectForProject(projectID), event); err != nil {
		return mapErr(err)
	}
	if err := c.nc.FlushTimeout(timeout); err != nil {
		return mapErr(err)
	}
	return nil
}

// OpsEventSubject is the operator-only event subject. Events published here are
// NOT relayed to tenants: elitea-main's natsbus EventBus subscribes per project
// under gateway.events.project.<id>.events, and this subject deliberately sits
// outside that tree.
//
// Used for budget.unbilled_stream (issue #9): telling a tenant in real time
// which of their streams the gateway failed to bill is an oracle for the
// conditions that produce it, so the loss record is operator-facing even though
// budget.soft_alert on the project subject is not.
const OpsEventSubject = EventSubjectRoot + ".ops.budget"

// PublishOpsEvent publishes a pre-marshalled envelope onto the operator-only
// subject. Bounded exactly like PublishSoftAlertEvent. Satisfies
// llmproxy.OpsEventPublisher.
func (c *Client) PublishOpsEvent(ctx context.Context, event []byte) error {
	timeout := OpTimeout
	if dl, ok := ctx.Deadline(); ok {
		if until := time.Until(dl); until < timeout {
			timeout = until
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	if err := c.nc.Publish(OpsEventSubject, event); err != nil {
		return mapErr(err)
	}
	if err := c.nc.FlushTimeout(timeout); err != nil {
		return mapErr(err)
	}
	return nil
}

// JetStream exposes the underlying JetStream handle for the scheduler's
// write-behind consumer wiring (§8.6). The gateway itself does not consume.
func (c *Client) JetStream() jetstream.JetStream { return c.js }

// BudgetSubject exposes the package-level BudgetSubject formula as a method so
// *Client satisfies the failmode.Counter interface and the server.NATSClient
// interface (both require a BudgetSubject method). The implementation delegates
// to the package function so the formula stays a single source of truth.
func (c *Client) BudgetSubject(scope, scopeID string, periodStartUnix int64) string {
	return BudgetSubject(scope, scopeID, periodStartUnix)
}

// Close tears down the NATS connection.
func (c *Client) Close() {
	if c.nc != nil && !c.nc.IsClosed() {
		c.nc.Close()
	}
}

// mapErr normalises breaker/timeout errors to ErrUnavailable so the governance
// store can distinguish an infrastructure failure (→ tiered-hybrid fallback)
// from a real error. Config/parse errors pass through unchanged.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gobreaker.ErrOpenState),
		errors.Is(err, gobreaker.ErrTooManyRequests),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.Is(err, nats.ErrTimeout),
		errors.Is(err, nats.ErrNoResponders),
		errors.Is(err, nats.ErrConnectionClosed):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		return err
	}
}
