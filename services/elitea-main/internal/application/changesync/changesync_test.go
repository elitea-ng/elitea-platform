package changesync

import (
	"encoding/base64"
	"errors"
	"math"
	"net/url"
	"testing"
	"time"
)

var dbNow = time.Date(2026, 10, 4, 12, 0, 0, 123456000, time.UTC)

func TestCursorRoundTrip(t *testing.T) {
	in := Cursor{
		Stream: StreamMessages,
		Scope:  "7/42",
		Rows:   Position{At: dbNow.Add(-time.Minute), ID: 9},
		Tombs:  Position{At: dbNow.Add(-2 * time.Minute), ID: 3},
	}
	out, err := Decode(Encode(in), StreamMessages, "7/42")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Full || !out.Rows.At.Equal(in.Rows.At) || out.Rows.ID != 9 ||
		!out.Tombs.At.Equal(in.Tombs.At) || out.Tombs.ID != 3 {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}

func TestDecodeFullSync(t *testing.T) {
	for _, raw := range []string{"", "0", " 0 "} {
		cursor, err := Decode(raw, StreamConversations, "1/x")
		if err != nil || !cursor.Full {
			t.Fatalf("Decode(%q) = %+v, %v; want a full sync", raw, cursor, err)
		}
		if !cursor.RowsStart().At.IsZero() || cursor.RowsStart().ID != 0 {
			t.Fatalf("a full sync must read rows from the beginning, got %+v", cursor.RowsStart())
		}
		bound := SettleBound(dbNow)
		if got := cursor.TombstonesStart(bound); !got.At.Equal(bound) {
			t.Fatalf("a full sync must read tombstones from the settle bound, got %+v", got)
		}
		if err := cursor.CheckExpiry(dbNow.Add(1000 * 24 * time.Hour)); err != nil {
			t.Fatalf("a full sync never expires: %v", err)
		}
	}
}

func TestDecodeRefusesForeignCursors(t *testing.T) {
	valid := Encode(Cursor{Stream: StreamConversations, Scope: "1/abc", Rows: Position{At: dbNow}, Tombs: Position{At: dbNow}})
	wrongVersion := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"v":2,"s":"conv","scope":"1/abc","t":"2026-10-04T12:00:00Z","i":0,"dt":"2026-10-04T12:00:00Z","di":0}`))
	badTime := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"v":1,"s":"conv","scope":"1/abc","t":"yesterday","i":0,"dt":"2026-10-04T12:00:00Z","di":0}`))
	cases := map[string]struct{ raw, stream, scope string }{
		"wrong stream":  {valid, StreamMessages, "1/abc"},
		"wrong scope":   {valid, StreamConversations, "2/abc"},
		"wrong filter":  {valid, StreamConversations, "1/def"},
		"wrong version": {wrongVersion, StreamConversations, "1/abc"},
		"bad time":      {badTime, StreamConversations, "1/abc"},
		"not base64":    {"%%%", StreamConversations, "1/abc"},
		"not json":      {base64.RawURLEncoding.EncodeToString([]byte("nope")), StreamConversations, "1/abc"},
		"too long":      {string(make([]byte, 2000)), StreamConversations, "1/abc"},
	}
	for name, tc := range cases {
		if _, err := Decode(tc.raw, tc.stream, tc.scope); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
}

func TestCheckExpiryBoundary(t *testing.T) {
	edge := dbNow.Add(-TombstoneRetention)
	at := func(instant time.Time) Cursor {
		return Cursor{Stream: StreamNotifications, Scope: "1", Rows: Position{}, Tombs: Position{At: instant}}
	}
	if err := at(edge).CheckExpiry(dbNow); err != nil {
		t.Fatalf("a cursor exactly at the retention edge is still served: %v", err)
	}
	if err := at(edge.Add(-time.Microsecond)).CheckExpiry(dbNow); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("a cursor past the retention edge: err = %v, want ErrCursorExpired", err)
	}
	// The rows position is never swept, so an old one alone is no reason.
	old := Cursor{Stream: StreamNotifications, Scope: "1", Rows: Position{At: dbNow.Add(-10 * TombstoneRetention)}, Tombs: Position{At: dbNow}}
	if err := old.CheckExpiry(dbNow); err != nil {
		t.Fatalf("an old rows position must not expire a cursor: %v", err)
	}
	if TombstoneRetention != 97*24*time.Hour {
		t.Fatalf("TombstoneRetention = %v, want MaxOfflineRetentionDays (90) + 7 days", TombstoneRetention)
	}
}

func TestAdvanceSettleClamp(t *testing.T) {
	bound := SettleBound(dbNow)
	ceiling := Position{At: bound, ID: math.MaxInt64}
	start := Position{At: bound.Add(-time.Hour), ID: 5}

	// Drained: the cursor moves to the settle bound, not to the last row.
	settled := Position{At: bound.Add(-time.Minute), ID: 8}
	if next, more := Advance(start, &settled, false, bound); next != ceiling || more {
		t.Fatalf("drained page: got %+v more=%v, want the settle ceiling and no more", next, more)
	}
	// A row inside the window does not drag the cursor past the bound.
	fresh := Position{At: dbNow.Add(-time.Second), ID: 9}
	if next, more := Advance(start, &fresh, false, bound); next != ceiling || more {
		t.Fatalf("page ending in the window: got %+v more=%v", next, more)
	}
	// Truncated before the bound: resume after the last row, more pages.
	if next, more := Advance(start, &settled, true, bound); next != settled || !more {
		t.Fatalf("truncated settled page: got %+v more=%v, want last row and more", next, more)
	}
	// Truncated inside the window: clamp and stop, so a loop on has_more
	// cannot spin on rows that are still settling.
	if next, more := Advance(start, &fresh, true, bound); next != ceiling || more {
		t.Fatalf("truncated unsettled page: got %+v more=%v", next, more)
	}
	// Never backwards, even for a cursor ahead of the bound.
	ahead := Position{At: dbNow, ID: 1}
	if next, _ := Advance(ahead, nil, false, bound); next != ahead {
		t.Fatalf("cursor moved backwards: got %+v, want %+v", next, ahead)
	}
	// Empty page from a full sync.
	if next, more := Advance(Position{}, nil, false, bound); next != ceiling || more {
		t.Fatalf("empty full sync: got %+v more=%v", next, more)
	}
}

func TestLimit(t *testing.T) {
	cases := map[string]struct {
		want int
		ok   bool
	}{"": {DefaultLimit, true}, "5": {5, true}, "1000": {MaxLimit, true}, "0": {0, false}, "-1": {0, false}, "x": {0, false}}
	for raw, tc := range cases {
		values := url.Values{}
		if raw != "" {
			values.Set("limit", raw)
		}
		got, err := Limit(values)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("Limit(%q) = %d, %v; want %d ok=%v", raw, got, err, tc.want, tc.ok)
		}
	}
}

func TestRequested(t *testing.T) {
	if _, ok := Requested(url.Values{}); ok {
		t.Fatal("absent changes_since must be the legacy list")
	}
	if raw, ok := Requested(url.Values{"changes_since": {""}}); !ok || raw != "" {
		t.Fatalf("empty changes_since must be a full delta, got %q %v", raw, ok)
	}
}

func TestFilterDigestSeparatesFilters(t *testing.T) {
	a := FilterDigest(url.Values{"source": {"elitea"}}, "source", "hidden")
	b := FilterDigest(url.Values{"source": {"deepwiki"}}, "source", "hidden")
	c := FilterDigest(url.Values{"source": {"elitea"}, "limit": {"5"}}, "hidden", "source")
	if a == b {
		t.Fatal("different filters share a cursor scope")
	}
	if a != c {
		t.Fatal("parameter order or a non-filter parameter changed the scope")
	}
}

func TestHTTPError(t *testing.T) {
	if status, body, ok := HTTPError(ErrCursorExpired); !ok || status != 410 || body.Error != CodeCursorExpired {
		t.Fatalf("expired: %d %+v %v", status, body, ok)
	}
	if status, body, ok := HTTPError(ErrInvalidCursor); !ok || status != 400 || body.Error != CodeInvalidCursor {
		t.Fatalf("invalid: %d %+v %v", status, body, ok)
	}
	if _, _, ok := HTTPError(errors.New("db down")); ok {
		t.Fatal("an unrelated error must not map to a cursor refusal")
	}
}
