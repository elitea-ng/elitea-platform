package workspace

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

func chainSegment(seq uint64, kind string, prerequisites ...string) workspaceruntime.Segment {
	refs := []workspaceruntime.Ref{}
	for _, name := range []string{"refs/elitea/base", "refs/elitea/upstream", "refs/elitea/handoff/1", workspaceruntime.RefCloud} {
		refs = append(refs, workspaceruntime.Ref{Name: name, SHA: strings.Repeat("a", 40)})
	}
	return workspaceruntime.Segment{
		Seq: seq, Kind: kind, SHA256: strings.Repeat("b", 64), Bytes: 1 << 30,
		Prerequisites: prerequisites, Refs: refs,
	}
}

func TestValidatePlanAcceptsAChainAndRefusesAMalformedOne(t *testing.T) {
	tip := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name     string
		segments []workspaceruntime.Segment
		valid    bool
	}{
		{"compact only (Phase 1)", []workspaceruntime.Segment{chainSegment(3, workspaceruntime.KindCompact)}, true},
		{"compact then pushes", []workspaceruntime.Segment{
			chainSegment(3, workspaceruntime.KindCompact), chainSegment(4, workspaceruntime.KindPush, tip),
			chainSegment(5, workspaceruntime.KindPush, tip)}, true},
		{"pushes only, past the compact", []workspaceruntime.Segment{chainSegment(5, workspaceruntime.KindPush, tip)}, true},
		{"a compact after a push", []workspaceruntime.Segment{
			chainSegment(4, workspaceruntime.KindPush, tip), chainSegment(5, workspaceruntime.KindCompact)}, false},
		{"a push without prerequisites", []workspaceruntime.Segment{
			chainSegment(3, workspaceruntime.KindCompact), chainSegment(4, workspaceruntime.KindPush)}, false},
		{"a compact with prerequisites", []workspaceruntime.Segment{chainSegment(3, workspaceruntime.KindCompact, tip)}, false},
		{"out of order", []workspaceruntime.Segment{
			chainSegment(3, workspaceruntime.KindCompact), chainSegment(5, workspaceruntime.KindPush, tip),
			chainSegment(4, workspaceruntime.KindPush, tip)}, false},
		{"another kind", []workspaceruntime.Segment{chainSegment(3, "DELTA")}, false},
	} {
		err := validatePlan(workspaceruntime.FetchPlan{HeadSeq: 9, Segments: tc.segments})
		if (err == nil) != tc.valid {
			t.Errorf("%s: validatePlan = %v, want valid %t", tc.name, err, tc.valid)
		}
	}
	tooLong := []workspaceruntime.Segment{chainSegment(1, workspaceruntime.KindCompact)}
	for seq := uint64(2); len(tooLong) <= workspaceruntime.MaxPlanSegments; seq++ {
		tooLong = append(tooLong, chainSegment(seq, workspaceruntime.KindPush, tip))
	}
	if err := validatePlan(workspaceruntime.FetchPlan{Segments: tooLong}); err == nil {
		t.Fatal("a chain over MaxPlanSegments was accepted")
	}
}

// The largest plan validatePlan accepts (one COMPACT and the most PUSH, each
// with every ref and the most prerequisites) still fits one response frame
// of the control listener (80 KiB).
func TestTheLargestPlanFrameFitsTheResponseBound(t *testing.T) {
	prerequisites := []string{}
	for index := 0; index < workspaceruntime.MaxPrerequisites; index++ {
		prerequisites = append(prerequisites, strings.Repeat(string(rune('0'+index)), 40))
	}
	refs := []workspaceruntime.Ref{}
	for _, name := range []string{"refs/elitea/base", "refs/elitea/upstream", "refs/elitea/cloud"} {
		refs = append(refs, workspaceruntime.Ref{Name: name, SHA: strings.Repeat("f", 40)})
	}
	for n := 1; n <= 8; n++ {
		refs = append(refs, workspaceruntime.Ref{Name: "refs/elitea/handoff/12345678" + string(rune('0'+n)), SHA: strings.Repeat("e", 40)})
	}
	plan := workspaceruntime.FetchPlan{HeadSeq: 1 << 40, Refs: refs}
	first := chainSegment(1<<40, workspaceruntime.KindCompact)
	first.Refs = refs
	plan.Segments = append(plan.Segments, first)
	for seq := uint64(1<<40 + 1); len(plan.Segments) < workspaceruntime.MaxPlanSegments; seq++ {
		segment := chainSegment(seq, workspaceruntime.KindPush, prerequisites...)
		segment.Refs = refs
		plan.Segments = append(plan.Segments, segment)
	}
	if err := validatePlan(plan); err != nil {
		t.Fatalf("the largest plan is refused: %v", err)
	}
	frame := &runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Plan{Plan: planProto(plan)}}
	if size := proto.Size(frame); size >= 80<<10 {
		t.Fatalf("the largest plan frame is %d bytes, over the 80 KiB response bound", size)
	}
}
