package delegationnotice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const refusedJSON = `{"id":"call-1","kind":"refused","reason":"no_grant","code":"DELEGATION_GRANT_MISSING",
"edge":{"caller_id":2,"caller_name":"Agent2","callee_id":1,"callee_name":"Agent1","kind":"agent_tool"},
"missing":[{"kind":"toolkit","name":"Git"}]}`

func TestSanitizeRebuildsAClosedBoundedShape(t *testing.T) {
	notice, ok := Sanitize(json.RawMessage(refusedJSON))
	if !ok || notice.Edge.CalleeName != "Agent1" || notice.Missing[0].Name != "Git" || notice.Edge.CallerID != 2 {
		t.Fatalf("notice = %+v, %v", notice, ok)
	}
	// An unknown key (a credential value, say) never survives.
	smuggled := strings.Replace(refusedJSON, `"kind":"refused"`, `"kind":"refused","secret":"hunter2","tool_inputs":{"x":1}`, 1)
	notice, ok = Sanitize(json.RawMessage(smuggled))
	encoded, _ := json.Marshal(notice)
	if !ok || strings.Contains(string(encoded), "hunter2") || strings.Contains(string(encoded), "tool_inputs") {
		t.Fatalf("an unknown key survived: %s", encoded)
	}
	many := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, `{"kind":"toolkit","name":"T`+string(rune('a'+i%26))+`"}`)
	}
	big := strings.Replace(refusedJSON, `[{"kind":"toolkit","name":"Git"}]`, "["+strings.Join(many, ",")+"]", 1)
	if notice, _ = Sanitize(json.RawMessage(big)); len(notice.Missing) != maxMissing {
		t.Fatalf("missing = %d, want %d", len(notice.Missing), maxMissing)
	}
}

func TestSanitizeRefusesWhatIsNotANotice(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":           `[`,
		"no id":              strings.Replace(refusedJSON, `"id":"call-1",`, ``, 1),
		"unknown reason":     strings.Replace(refusedJSON, `no_grant`, `whatever`, 1),
		"unknown code":       strings.Replace(refusedJSON, `DELEGATION_GRANT_MISSING`, `OTHER`, 1),
		"unknown edge kind":  strings.Replace(refusedJSON, `agent_tool`, `skill`, 1),
		"narrowed not stale": strings.Replace(refusedJSON, `"kind":"refused"`, `"kind":"narrowed"`, 1),
		"stale not narrowed": strings.Replace(refusedJSON, `no_grant`, `stale`, 1),
	} {
		if _, ok := Sanitize(json.RawMessage(raw)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMergeKeepsOnePerCallAndIsBounded(t *testing.T) {
	first, _ := Sanitize(json.RawMessage(refusedJSON))
	var kept []Notice
	kept = Merge(kept, first)
	replaced := first
	replaced.Reason = "principal"
	kept = Merge(kept, replaced)
	if len(kept) != 1 || kept[0].Reason != "principal" {
		t.Fatalf("kept = %+v", kept)
	}
	for i := 0; i < 40; i++ {
		next := first
		next.ID = "call-" + string(rune('A'+i))
		kept = Merge(kept, next)
	}
	if len(kept) != MaxNotices {
		t.Fatalf("kept %d, want %d", len(kept), MaxNotices)
	}
}

func TestStampOffersReviewOnlyWhereAnEditorCanFixIt(t *testing.T) {
	cases := []struct {
		reason string
		review Review
		want   bool
		names  bool
	}{
		{"no_grant", Review{CanReview: true}, true, false},
		{"stale", Review{CanReview: true}, true, false},
		{"no_grant", Review{Editors: []string{"Ada", "Grace", "Linus", "Ken"}}, false, true},
		{"principal", Review{CanReview: true}, false, false},
		{"depth", Review{CanReview: true}, false, false},
	}
	for _, c := range cases {
		notice := map[string]any{"reason": c.reason}
		Stamp(notice, c.review)
		if notice["can_review"] != c.want {
			t.Errorf("%s %+v: can_review = %v", c.reason, c.review, notice["can_review"])
		}
		editors, has := notice["editors"].([]string)
		if has != c.names || (has && len(editors) != MaxEditors) {
			t.Errorf("%s %+v: editors = %v", c.reason, c.review, notice["editors"])
		}
	}
}

type fakeReviewer struct {
	review Review
	err    error
	calls  int
}

func (f *fakeReviewer) DelegationReview(context.Context, int64, int64, int64) (Review, error) {
	f.calls++
	return f.review, f.err
}

func TestStampFrameAddsTheViewersReviewAndNothingElse(t *testing.T) {
	frame := `{"type":"delegation_refused","stream_id":"s","response_metadata":{"delegation_v1":` + refusedJSON + `},"n":12345678901234567890}`
	out := StampFrame(t.Context(), []byte(frame), 7, 9, &fakeReviewer{review: Review{CanReview: true}})
	var decoded struct {
		ResponseMetadata struct {
			Notice map[string]any `json:"delegation_v1"`
		} `json:"response_metadata"`
		N json.Number `json:"n"`
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ResponseMetadata.Notice["can_review"] != true || decoded.N.String() != "12345678901234567890" {
		t.Fatalf("stamped = %s", out)
	}
	// A frame without a notice, an unreadable one and a failing reviewer pass through.
	for _, data := range []string{`{"type":"agent_tool_end"}`, `{"response_metadata":{"delegation_v1":`, frame} {
		reviewer := &fakeReviewer{err: errors.New("down")}
		if got := StampFrame(t.Context(), []byte(data), 7, 9, reviewer); string(got) != data {
			t.Errorf("%q changed to %q", data, got)
		}
	}
	if got := StampFrame(t.Context(), []byte(frame), 7, 9, nil); string(got) != frame {
		t.Error("a nil reviewer changed the frame")
	}
}

func TestStampMetaCopiesAndStamps(t *testing.T) {
	notice := map[string]any{"reason": "no_grant", "edge": map[string]any{"caller_id": float64(2)}}
	meta := map[string]any{MetaKey: []any{notice}}
	served := StampMeta(t.Context(), meta, 7, 9, &fakeReviewer{review: Review{Editors: []string{"Ada"}}})
	stamped := served[MetaKey].([]any)[0].(map[string]any)
	if stamped["can_review"] != false || stamped["editors"].([]string)[0] != "Ada" {
		t.Fatalf("stamped = %+v", stamped)
	}
	if _, touched := notice["can_review"]; touched {
		t.Fatal("the stored notice was modified in place")
	}
	if _, touched := meta[MetaKey].([]any)[0].(map[string]any)["can_review"]; touched {
		t.Fatal("the map the caller passed (it may be shared) was modified")
	}
}

type fakePermissions struct {
	allowed bool
	err     error
}

func (f fakePermissions) HasProjectPermission(context.Context, int64, int64, string) (bool, error) {
	return f.allowed, f.err
}

type fakeEditors struct {
	names []string
	err   error
}

func (f fakeEditors) EditorNames(context.Context, int64, int64, int) ([]string, error) {
	return f.names, f.err
}

func TestServiceReviewIsTheEditPermissionAndOwnersOtherwise(t *testing.T) {
	ctx := t.Context()
	editors := fakeEditors{names: []string{"Ada"}}
	review, err := NewService(fakePermissions{allowed: true}, editors).DelegationReview(ctx, 1, 2, 3)
	if err != nil || !review.CanReview || len(review.Editors) != 0 {
		t.Fatalf("an editor: %+v %v", review, err)
	}
	review, _ = NewService(fakePermissions{}, editors).DelegationReview(ctx, 1, 2, 3)
	if review.CanReview || len(review.Editors) != 1 {
		t.Fatalf("a reader: %+v", review)
	}
	review, _ = NewService(fakePermissions{allowed: true}, editors).DelegationReview(ctx, 1, 2, 0)
	if review.CanReview {
		t.Fatal("an ad hoc caller has no grant to review")
	}
	review, err = NewService(fakePermissions{}, fakeEditors{err: errors.New("down")}).DelegationReview(ctx, 1, 2, 3)
	if err != nil || review.CanReview || len(review.Editors) != 0 {
		t.Fatalf("a failed owner lookup: %+v %v", review, err)
	}
	if _, err = NewService(fakePermissions{err: errors.New("down")}, editors).DelegationReview(ctx, 1, 2, 3); err == nil {
		t.Fatal("a failed permission check was read as an answer")
	}
	if NewService(nil, editors) != nil || NewService(fakePermissions{}, nil) != nil {
		t.Fatal("a service without a side was built")
	}
}

func TestCachedAsksOncePerCaller(t *testing.T) {
	inner := &fakeReviewer{review: Review{CanReview: true}}
	reviewer := Cached(inner)
	for i := 0; i < 3; i++ {
		if _, err := reviewer.DelegationReview(t.Context(), 1, 2, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reviewer.DelegationReview(t.Context(), 1, 2, 4); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want one per caller", inner.calls)
	}
	if Cached(nil) != nil {
		t.Fatal("a nil reviewer was wrapped")
	}
}

func TestDecodeDropsWhatIsNotANotice(t *testing.T) {
	stored := `[` + refusedJSON + `,{"id":"x"},"junk"]`
	if got := Decode([]byte(stored)); len(got) != 1 || got[0].ID != "call-1" {
		t.Fatalf("decoded = %+v", got)
	}
	if got := Decode([]byte(`{"not":"a list"}`)); got != nil {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestSanitizeNamesAnUnnamedCalleeRatherThanHidingTheRefusal(t *testing.T) {
	raw := strings.Replace(refusedJSON, `"callee_name":"Agent1"`, `"callee_name":"  \u0007 "`, 1)
	notice, ok := Sanitize(json.RawMessage(raw))
	if !ok || notice.Edge.CalleeName != UnnamedAgent {
		t.Fatalf("notice = %+v, %v", notice, ok)
	}
}

type namingEditors struct {
	fakeEditors
	name string
}

func (n namingEditors) AgentName(context.Context, int64, int64) (string, error) { return n.name, nil }

func TestStampNamesTheCallingAgentWithoutTouchingTheSharedEdge(t *testing.T) {
	edge := map[string]any{"caller_id": float64(2), "caller_name": "elitea-agent"}
	meta := map[string]any{MetaKey: []any{map[string]any{"reason": "no_grant", "edge": edge}}}
	review, err := NewService(fakePermissions{allowed: true}, namingEditors{name: "Release helper"}).DelegationReview(t.Context(), 1, 2, 2)
	if err != nil || review.CallerName != "Release helper" {
		t.Fatalf("review = %+v %v", review, err)
	}
	served := StampMeta(t.Context(), meta, 1, 2, &fakeReviewer{review: review})
	got := served[MetaKey].([]any)[0].(map[string]any)["edge"].(map[string]any)["caller_name"]
	if got != "Release helper" {
		t.Fatalf("caller_name = %v", got)
	}
	if edge["caller_name"] != "elitea-agent" {
		t.Fatal("the shared edge map was modified")
	}
	frame := `{"response_metadata":{"delegation_v1":` + refusedJSON + `}}`
	out := StampFrame(t.Context(), []byte(frame), 1, 2, &fakeReviewer{review: Review{CallerName: "Release helper"}})
	if !strings.Contains(string(out), `"caller_name":"Release helper"`) {
		t.Fatalf("a live frame keeps the runtime label: %s", out)
	}
}
