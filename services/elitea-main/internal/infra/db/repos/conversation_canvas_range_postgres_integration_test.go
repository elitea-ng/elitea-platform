package repos

// The canvas selection's BOUNDS.
//
// `CreateCanvas` slices the stored message text with `content[startsAt:endsAt]`,
// and both numbers come off the request body: the client computes them over the
// copy of the answer it is rendering and sends the pair. Only one relationship
// between them was checked — start must not be after end — so every other bad
// pair reached the slice, and Go's slice expression PANICS on an index outside
// the string rather than returning an error. The caller then read a 500 for a
// selection it made, with nothing in the body naming which end was wrong, and
// the panic reached the recovery middleware instead of the route.
//
// Two inputs produce it, and neither is exotic:
//
//   - an END past the last byte, which is what a client sends after the message
//     it measured was regenerated or edited to something shorter;
//   - a NEGATIVE start, which is what an unset or mis-derived offset looks like
//     once `intFromAny` has read it.
//
// The pair `startsAt > endsAt` is checked before the message is even read, so
// it is refused on any input; these two need the message's own length and are
// therefore checked after the row is loaded. All three are the same 400.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL); `newFreshInstallPool`
// skips without one.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

func TestCreateCanvasRefusesASelectionOutsideTheMessage(t *testing.T) {
	pool := newFreshInstallPool(t)
	const seeded = "before CANVAS BODY after"
	groupID, itemID, _ := seedFreshInstallTextItem(t, pool, seeded)

	repo := NewConversationsRepo(pool)

	cases := []struct {
		name     string
		startsAt int
		endsAt   int
	}{
		{"an end past the last byte of the message", 0, len(seeded) + 1},
		{"a selection that begins inside and runs off the end", 7, len(seeded) * 3},
		{"a negative start", -5, 3},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// A panic here is the defect this test exists for, and it would
			// otherwise fail the whole package rather than this case.
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("CreateCanvas panicked on %s: %v", testCase.name, recovered)
				}
			}()

			canvas, err := repo.CreateCanvas(ctx, "1", map[string]any{
				"message_group_id":         groupID,
				"message_item_id":          itemID,
				"name":                     "out of range",
				"canvas_type":              "code",
				"canvas_content_starts_at": testCase.startsAt,
				"canvas_content_ends_at":   testCase.endsAt,
			})
			if err == nil {
				t.Fatalf("CreateCanvas accepted %s and answered %v", testCase.name, canvas)
			}
			var apiError *apierr.APIError
			if !errors.As(err, &apiError) || apiError.Status != 400 {
				t.Errorf("err=%v for %s, want a 400 API error: a selection the client sent is caller input", err, testCase.name)
			}
		})
	}

	// The message is UNTOUCHED. The split deletes the original text item before
	// it writes anything, so a refusal that arrived after the delete would have
	// destroyed the answer it declined to carve.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var content string
	if err := pool.QueryRow(ctx,
		`SELECT content FROM p_1.chat_messages_text WHERE id = $1`, itemID).Scan(&content); err != nil {
		t.Fatalf("read the message back after the refusals: %v", err)
	}
	if content != seeded {
		t.Errorf("stored content=%q, want the message the refusals left alone (%q)", content, seeded)
	}

	// …and a selection that IS inside the message still works, so the guard
	// refuses the range and not the route.
	if _, err := repo.CreateCanvas(ctx, "1", map[string]any{
		"message_group_id":         groupID,
		"message_item_id":          itemID,
		"name":                     "in range",
		"canvas_type":              "code",
		"canvas_content_starts_at": 7,
		"canvas_content_ends_at":   18,
	}); err != nil {
		t.Fatalf("CreateCanvas refused a selection inside the message: %v", err)
	}
}
