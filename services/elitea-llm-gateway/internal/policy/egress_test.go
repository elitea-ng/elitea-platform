package policy

import (
	"strings"
	"testing"
	"time"
)

func egressRow(name string, entries ...any) Row {
	return Row{
		ID:      "row-" + name,
		Type:    TypeEgressAllowlist,
		Section: "governance",
		Name:    name,
		Enabled: true,
		Data:    map[string]any{"egress": map[string]any{"allowlist": entries}},
	}
}

func TestCompileEgressAllowlist(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		egressRow("on-prem", "192.168.29.60:8000", "192.168.29.0/24"),
		egressRow("azure", "*.openai.azure.com"),
	}, time.Now())

	if len(snap.Rejected) != 0 {
		t.Fatalf("rejected = %v, want none", snap.Rejected)
	}
	got := strings.Join(snap.EgressAllowlist(), ",")
	want := "*.openai.azure.com,192.168.29.0/24,192.168.29.60:8000"
	if got != want {
		t.Fatalf("EgressAllowlist() = %q, want %q", got, want)
	}
	if snap.Diagnostics().EgressAllowlists != 2 {
		t.Fatalf("diagnostics EgressAllowlists = %d, want 2", snap.Diagnostics().EgressAllowlists)
	}
}

// TestCompileEgressAllowlistUnionsEveryRow: every row is an operator statement
// that a destination is legitimate. A second row must not silently revoke the
// first, so the result is the union and not the narrowest match.
func TestCompileEgressAllowlistUnionsEveryRow(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		egressRow("a", "a.example.com"),
		egressRow("b", "b.example.com", "a.example.com"),
	}, time.Now())

	got := strings.Join(snap.EgressAllowlist(), ",")
	if got != "a.example.com,b.example.com" {
		t.Fatalf("EgressAllowlist() = %q, want the deduplicated union", got)
	}
}

// TestCompileEgressAllowlistRejectsAMalformedEntry: one bad entry rejects the
// WHOLE row. A row that silently lost a member would refuse a destination the
// operator believes they permitted, while looking correct in the admin list.
func TestCompileEgressAllowlistRejectsAMalformedEntry(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{egressRow("bad", "good.example.com", "https://nope/path")}, time.Now())

	if len(snap.Rejected) != 1 {
		t.Fatalf("rejected = %v, want exactly one row", snap.Rejected)
	}
	if snap.Rejected[0].Type != TypeEgressAllowlist || snap.Rejected[0].Name != "bad" {
		t.Fatalf("rejected row = %+v, want the egress row named", snap.Rejected[0])
	}
	if !strings.Contains(snap.Rejected[0].Reason, "egress.allowlist") {
		t.Fatalf("rejection reason %q does not name the field", snap.Rejected[0].Reason)
	}
	if len(snap.EgressAllowlist()) != 0 {
		t.Fatalf("a rejected row still contributed entries: %v", snap.EgressAllowlist())
	}
}

// TestCompileEgressAllowlistRejectsAScope: an egress row is global. The
// private-network half of the decision is made in GetConfigForProvider, which
// bifrost calls with no project, so a scoped row could only be half-honoured.
// Applying it globally would widen it beyond what was authored.
func TestCompileEgressAllowlistRejectsAScope(t *testing.T) {
	t.Parallel()

	row := egressRow("scoped", "a.example.com")
	row.Data["scope"] = map[string]any{"project_ids": []any{float64(7)}}
	snap := Compile([]Row{row}, time.Now())

	if len(snap.Rejected) != 1 {
		t.Fatalf("rejected = %v, want the scoped row refused", snap.Rejected)
	}
	if !strings.Contains(snap.Rejected[0].Reason, "global") {
		t.Fatalf("rejection reason %q does not explain that the row is global", snap.Rejected[0].Reason)
	}
	if len(snap.EgressAllowlist()) != 0 {
		t.Fatal("a scoped row was applied globally, which is wider than it was authored")
	}
}

// TestCompileEgressAllowlistEmptyIsInert names the row rather than dropping it.
// An empty list here is NOT the mcp_allowlist meaning: it permits nothing and
// widens nothing, so an operator who cleared the field must be told they
// removed a permission rather than a restriction.
func TestCompileEgressAllowlistEmptyIsInert(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{egressRow("empty")}, time.Now())

	if len(snap.Rejected) != 0 {
		t.Fatalf("rejected = %v, want none", snap.Rejected)
	}
	if len(snap.Inert) != 1 {
		t.Fatalf("inert = %v, want the empty row named", snap.Inert)
	}
	if !strings.Contains(snap.Inert[0].Reason, "permits no destination") {
		t.Fatalf("inert reason %q does not state what an empty list does", snap.Inert[0].Reason)
	}
	if len(snap.EgressAllowlist()) != 0 {
		t.Fatalf("an empty row contributed entries: %v", snap.EgressAllowlist())
	}
}

// TestCompileEgressAllowlistAcceptsTheFlatShape covers a hand-written row and
// the older top-level shape, exactly as every other type here does.
func TestCompileEgressAllowlistAcceptsTheFlatShape(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{{
		ID: "flat", Type: TypeEgressAllowlist, Name: "flat", Enabled: true,
		Data: map[string]any{"allowlist": []any{"a.example.com"}},
	}}, time.Now())

	if len(snap.EgressAllowlist()) != 1 {
		t.Fatalf("EgressAllowlist() = %v, want the flat shape read", snap.EgressAllowlist())
	}
}

// TestEgressAllowlistOnAnEmptySnapshot pins the enforcement-neutral posture the
// gateway holds before its first successful read.
func TestEgressAllowlistOnAnEmptySnapshot(t *testing.T) {
	t.Parallel()

	if got := Empty.EgressAllowlist(); len(got) != 0 {
		t.Fatalf("Empty.EgressAllowlist() = %v, want none", got)
	}
	var nilSnap *Snapshot
	if got := nilSnap.EgressAllowlist(); len(got) != 0 {
		t.Fatalf("nil snapshot EgressAllowlist() = %v, want none", got)
	}
	var nilStore *Store
	if got := nilStore.EgressAllowlist(); len(got) != 0 {
		t.Fatalf("nil store EgressAllowlist() = %v, want none", got)
	}
}

// TestEgressAllowlistIsNotAnUnknownType is the guard the budget_alert comment in
// policy.go describes: a type the compiler does not name is logged as an
// unrecognised definition on every refresh.
func TestEgressAllowlistIsNotAnUnknownType(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{egressRow("known", "a.example.com")}, time.Now())
	for _, r := range snap.Rejected {
		if strings.Contains(r.Reason, "unknown governance type") {
			t.Fatalf("egress_allowlist reads as an unknown type: %q", r.Reason)
		}
	}
}
