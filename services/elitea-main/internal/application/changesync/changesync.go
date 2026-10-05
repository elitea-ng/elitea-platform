// Package changesync is the cursor arithmetic behind `changes_since`
// (ADR-0025 WP6): the opaque cursor codec, the settle-window clamp, the
// expiry rule that answers 410, and the shared wire shapes of a delta page.
//
// # The cursor
//
// A cursor is base64url(JSON) with a version, the stream it belongs to
// (`conv`, `msg`, `notif`), the scope it was issued for (`<project>`,
// `<project>/<conversation id>`, `<user>`, plus a digest of the list filters
// for conversations) and two positions: one in the rows stream, ordered by
// (sync_at, id), and one in the tombstone stream, ordered by (deleted_at, id).
// A cursor presented for another stream or scope is a 400. It carries no MAC:
// every row and tombstone a delta returns still passes the caller's
// visibility predicate, so a forged cursor reads nothing the caller could not
// list from the beginning.
//
// # The settle window
//
// Timestamps are stamped by triggers when a statement runs and become visible
// when its transaction commits, so a row stamped at T1 can commit after a row
// stamped at T2 > T1 was already returned. A cursor that moved past T2 would
// skip it forever. A returned cursor therefore never moves past
// now - SettleWindow: anything stamped within the window comes back again on
// the next call, and clients upsert by id. Writes in this platform are short
// transactions; one that stays open longer than the window between stamping
// and committing is the residual risk the window accepts.
//
// # Retention and 410
//
// Rows are never removed by retention, so the rows position may be arbitrarily
// old. Tombstones are: elitea-scheduler deletes them after TombstoneRetention.
// A cursor whose tombstone position is older than that may have missed a
// deletion, so it answers 410 `sync_cursor_expired` and the client resyncs
// from scratch. A full sync (`changes_since=` or `0`) starts its tombstone
// position at now - SettleWindow, not at the beginning of time: a client
// that has nothing cached needs no tombstone older than its first request.
package changesync

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Param is the query parameter that switches a list into delta mode.
const Param = "changes_since"

// Streams.
const (
	StreamConversations = "conv"
	StreamMessages      = "msg"
	StreamNotifications = "notif"
)

const (
	// SettleWindow is how far behind the database clock a returned cursor
	// stays (see the package doc). The child -> parent bump throttle in tenant
	// migration 0144 (1 s) must stay shorter than this.
	SettleWindow = 5 * time.Second

	// TombstoneRetention is how long tombstones are kept and therefore the
	// oldest usable cursor: the longest offline_retention_days a deployment
	// may set (platformconfig.MaxOfflineRetentionDays) plus seven days of
	// margin. elitea-scheduler's syncretention sweeper keeps its own copy of
	// this number (a separate module) and never sweeps below it.
	TombstoneRetention = time.Duration(platformconfig.MaxOfflineRetentionDays+7) * 24 * time.Hour

	// DefaultLimit and MaxLimit bound one delta page, rows and tombstones
	// each.
	DefaultLimit = 100
	MaxLimit     = 100

	cursorVersion = 1
)

// Error codes on the wire.
const (
	CodeInvalidCursor = "invalid_sync_cursor"
	CodeCursorExpired = "sync_cursor_expired"
)

var (
	// ErrInvalidCursor is a cursor that does not decode, has another version,
	// or was issued for another stream or scope. HTTP 400.
	ErrInvalidCursor = errors.New(CodeInvalidCursor)
	// ErrCursorExpired is a cursor older than TombstoneRetention. HTTP 410.
	ErrCursorExpired = errors.New(CodeCursorExpired)
)

// Position is a place in one ordered stream: rows with (stamp, id) greater
// than it come next.
type Position struct {
	At time.Time
	ID int64
}

// After reports whether p is strictly later than other in (At, ID) order.
func (p Position) After(other Position) bool {
	if p.At.Equal(other.At) {
		return p.ID > other.ID
	}
	return p.At.After(other.At)
}

// Cursor is a decoded `changes_since` value.
type Cursor struct {
	Stream string
	Scope  string
	// Full is a sync from the beginning (`changes_since=` or `0`).
	Full  bool
	Rows  Position
	Tombs Position
}

type wireCursor struct {
	V     int    `json:"v"`
	S     string `json:"s"`
	Scope string `json:"scope"`
	T     string `json:"t"`
	I     int64  `json:"i"`
	DT    string `json:"dt"`
	DI    int64  `json:"di"`
}

// Requested reports whether the query asks for delta mode, and the raw value.
func Requested(values url.Values) (string, bool) {
	raw, ok := values[Param]
	if !ok {
		return "", false
	}
	if len(raw) == 0 {
		return "", true
	}
	return raw[0], true
}

// Decode parses a `changes_since` value for the given stream and scope. An
// empty value or "0" is a full sync.
func Decode(raw, stream, scope string) (Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return Cursor{Stream: stream, Scope: scope, Full: true}, nil
	}
	if len(raw) > 1024 {
		return Cursor{}, ErrInvalidCursor
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	var wire wireCursor
	if err := json.Unmarshal(body, &wire); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	if wire.V != cursorVersion || wire.S != stream || wire.Scope != scope {
		return Cursor{}, ErrInvalidCursor
	}
	rowsAt, err := time.Parse(time.RFC3339Nano, wire.T)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	tombsAt, err := time.Parse(time.RFC3339Nano, wire.DT)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{
		Stream: stream,
		Scope:  scope,
		Rows:   Position{At: rowsAt.UTC(), ID: wire.I},
		Tombs:  Position{At: tombsAt.UTC(), ID: wire.DI},
	}, nil
}

// Encode renders a cursor. Full is not encoded: an encoded cursor is always
// a position.
func Encode(cursor Cursor) string {
	body, _ := json.Marshal(wireCursor{
		V:     cursorVersion,
		S:     cursor.Stream,
		Scope: cursor.Scope,
		T:     cursor.Rows.At.UTC().Format(time.RFC3339Nano),
		I:     cursor.Rows.ID,
		DT:    cursor.Tombs.At.UTC().Format(time.RFC3339Nano),
		DI:    cursor.Tombs.ID,
	})
	return base64.RawURLEncoding.EncodeToString(body)
}

// CheckExpiry answers ErrCursorExpired for a cursor whose tombstone position
// is older than TombstoneRetention at dbNow.
func (c Cursor) CheckExpiry(dbNow time.Time) error {
	if c.Full {
		return nil
	}
	if c.Tombs.At.Before(dbNow.Add(-TombstoneRetention)) {
		return ErrCursorExpired
	}
	return nil
}

// SettleBound is the latest instant a returned cursor may name.
func SettleBound(dbNow time.Time) time.Time {
	return dbNow.Add(-SettleWindow)
}

// RowsStart is where the rows stream reads from.
func (c Cursor) RowsStart() Position {
	if c.Full {
		return Position{}
	}
	return c.Rows
}

// TombstonesStart is where the tombstone stream reads from: a full sync starts
// at the settle bound (see the package doc).
func (c Cursor) TombstonesStart(bound time.Time) Position {
	if c.Full {
		return Position{At: bound}
	}
	return c.Tombs
}

// Advance computes the next position of one stream.
//
// start is where this page read from; last is the position of the last item
// returned (nil when none); truncated reports that the stream had more items
// than the page carried. The result never names an instant after bound, and
// never moves backwards. more is true only when the stream was cut short
// strictly before the settle bound, so a caller that loops while more cannot
// spin on items that are still settling.
func Advance(start Position, last *Position, truncated bool, bound time.Time) (Position, bool) {
	ceiling := Position{At: bound, ID: math.MaxInt64}
	if truncated && last != nil && !last.After(ceiling) {
		return *last, true
	}
	if ceiling.After(start) {
		return ceiling, false
	}
	return start, false
}

// Limit parses `limit` for a delta page: DefaultLimit when absent, an error
// when not a positive integer, MaxLimit at most.
func Limit(values url.Values) (int, error) {
	raw := values.Get("limit")
	if raw == "" {
		return DefaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, errors.New("invalid limit")
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	return limit, nil
}

// FilterDigest is a short digest of the named query parameters, folded into a
// conversation cursor's scope so a cursor is not reused under another filter
// (a different filter is a different list, with its own lost-access events).
func FilterDigest(values url.Values, names ...string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var builder strings.Builder
	for _, name := range sorted {
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(values.Get(name))
		builder.WriteByte('&')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:6])
}

// Tombstone reasons.
const (
	ReasonDeleted    = "deleted"
	ReasonAccessLost = "access_lost"
)

// Tombstone is one removal on the wire. `id` is the removed item's integer id
// (for messages: the message group id the legacy list spells as a string);
// `uuid` is null only where the store never had one.
type Tombstone struct {
	ID        int64   `json:"id"`
	UUID      *string `json:"uuid"`
	Reason    string  `json:"reason"`
	DeletedAt string  `json:"deleted_at"`
}

// FormatTime is the wire spelling of a tombstone instant.
func FormatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// ErrorBody is the JSON body of a 400 or 410 cursor refusal.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// HTTPError maps a cursor error to its status and body; ok is false for any
// other error.
func HTTPError(err error) (int, ErrorBody, bool) {
	switch {
	case errors.Is(err, ErrCursorExpired):
		return 410, ErrorBody{Error: CodeCursorExpired,
			Message: "the sync cursor is older than the tombstone retention; resync from changes_since=0"}, true
	case errors.Is(err, ErrInvalidCursor):
		return 400, ErrorBody{Error: CodeInvalidCursor,
			Message: "changes_since is not a cursor this list issued"}, true
	}
	return 0, ErrorBody{}, false
}
