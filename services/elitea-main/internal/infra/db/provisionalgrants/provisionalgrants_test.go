package provisionalgrants

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// presence answers Present's query with a fixed answer.
type presence struct {
	present bool
	probes  int
}

type boolRow bool

func (r boolRow) Scan(dest ...any) error {
	*(dest[0].(*bool)) = bool(r)
	return nil
}

func (p *presence) QueryRow(context.Context, string, ...any) pgx.Row {
	p.probes++
	return boolRow(p.present)
}

// A writer switches the readers on once the table exists, refuses while it
// does not, and never switches them off.
func TestEnsureEnabledSwitchesOnOnceTheTableExists(t *testing.T) {
	previous := Enable(false)
	t.Cleanup(func() { Enable(previous) })

	absent := &presence{}
	if err := EnsureEnabled(t.Context(), absent); !errors.Is(err, ErrUnavailable) || Enabled() {
		t.Fatalf("without the table: %v (enabled %v), want %v and off", err, Enabled(), ErrUnavailable)
	}
	if RoleRows("$1") == "" || MembershipRows("$1") == "" {
		t.Fatal("a disabled fragment is empty text")
	}
	present := &presence{present: true}
	if err := EnsureEnabled(t.Context(), present); err != nil || !Enabled() {
		t.Fatalf("with the table: %v (enabled %v), want on", err, Enabled())
	}
	if err := EnsureEnabled(t.Context(), absent); err != nil || !Enabled() || absent.probes != 1 {
		t.Fatalf("once on: %v (enabled %v, %d probes), want on without a probe", err, Enabled(), absent.probes)
	}
}
