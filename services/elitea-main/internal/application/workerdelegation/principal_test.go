package workerdelegation

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// principalStub is a user with partial access: the closure holds toolkits
// 7, 8 and 9 and the folder access hides 9 from the user.
type principalStub struct {
	principal   Principal
	reachable   []int64
	hidden      []int64
	credentials []string
	seeds       ClosureSeeds
	hiddenFor   int64
	err         error
	reads       int
}

func (s *principalStub) RunPrincipal(context.Context, *runtimev1.WorkerCommandV1) (Principal, error) {
	return s.principal, s.err
}

func (s *principalStub) ReachableToolkits(_ context.Context, _ int64, seeds ClosureSeeds) ([]int64, error) {
	s.reads++
	s.seeds = seeds
	return s.reachable, s.err
}

func (s *principalStub) HiddenToolkits(_ context.Context, _, userID int64, _ []int64) ([]int64, error) {
	s.hiddenFor = userID
	return s.hidden, s.err
}

func (s *principalStub) UsableCredentialReferences(context.Context, int64, []int64) ([]string, error) {
	return s.credentials, s.err
}

func boundedSettings() platformconfig.DelegationSettings {
	settings := savedDefaults()
	settings.PrincipalBound = true
	return settings
}

func boundedResolver(t *testing.T, settings platformconfig.DelegationSettings, stub *principalStub) *Resolver {
	t.Helper()
	r, _ := resolver(t, settings, agentInput(t, `{"id":5,"version_id":9,"version_details":{"tools":[
		{"id":7,"type":"jira"},
		{"id":55,"type":"application","settings":{"application_id":"6","application_version_id":"10"}}]}}`), nil)
	return r.WithPrincipalBound(stub)
}

func toolkitIDs(bound *runtimev1.WorkerPrincipalBoundV1) []uint64 {
	var ids []uint64
	for _, toolkit := range bound.GetToolkits() {
		ids = append(ids, toolkit.GetToolkitId())
	}
	return ids
}

// A user with partial access: the toolkit the project hides from them is not
// in the bound, and the rest of the closure is, with every tool.
func TestTheBoundHoldsWhatTheUserMayUseOfTheClosure(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: []int64{9, 7, 8}, hidden: []int64{9}}
	policy, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	bound := policy.GetPrincipalBound()
	if !bound.GetBounded() || !slices.Equal(toolkitIDs(bound), []uint64{7, 8}) {
		t.Fatalf("bound = %v", bound)
	}
	for _, toolkit := range bound.GetToolkits() {
		if !toolkit.GetAllTools() {
			t.Fatalf("toolkit %d is not all tools", toolkit.GetToolkitId())
		}
	}
	if bound.GetCredentialsBounded() || len(bound.GetDataClasses()) != 0 {
		t.Fatalf("an ungoverned run has no credential or data class bound: %v", bound)
	}
	if stub.hiddenFor != 31 {
		t.Fatalf("hidden asked for user %d, want 31", stub.hiddenFor)
	}
}

// The closure is bounded: the walk starts at what the input names, never at
// the whole project.
func TestTheClosureStartsAtTheInputsRootAndAttachments(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}}
	if _, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stub.seeds.ToolkitIDs, []int64{7}) {
		t.Fatalf("seed toolkits = %v", stub.seeds.ToolkitIDs)
	}
	want := []applicationcomposition.Identity{{ApplicationID: 5, VersionID: 9}, {ApplicationID: 6, VersionID: 10}}
	if !slices.Equal(stub.seeds.Versions, want) {
		t.Fatalf("seed versions = %v, want %v", stub.seeds.Versions, want)
	}
}

// An ad hoc conversation names no saved agent: its own tools are the seeds.
func TestAnAdHocConversationIsBoundedToo(t *testing.T) {
	t.Parallel()
	content, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{
		Tools: []byte(`[{"id":12,"type":"github"},{"id":"13","type":"jira"},{"type":"web_search"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := resolver(t, boundedSettings(), content, nil)
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: []int64{12, 13}}
	policy, err := r.WithPrincipalBound(stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stub.seeds.ToolkitIDs, []int64{12, 13}) || len(stub.seeds.Versions) != 0 {
		t.Fatalf("seeds = %+v", stub.seeds)
	}
	if !policy.GetPrincipalBound().GetBounded() || !slices.Equal(toolkitIDs(policy.GetPrincipalBound()), []uint64{12, 13}) {
		t.Fatalf("bound = %v", policy.GetPrincipalBound())
	}
}

// A user who may use nothing of the closure still gets a bounded, empty
// bound: absent would read as unbounded.
func TestAnEmptyBoundIsStillBounded(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: []int64{9}, hidden: []int64{9}}
	policy, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	bound := policy.GetPrincipalBound()
	if bound == nil || !bound.GetBounded() || len(bound.GetToolkits()) != 0 {
		t.Fatalf("bound = %v; want bounded and empty", bound)
	}
}

// A governed run adds the credential references some use grant lets a toolkit
// of the closure use; a mandate acts for its subject user.
func TestAGovernedRunBoundsCredentialReferences(t *testing.T) {
	t.Parallel()
	stub := &principalStub{
		principal: Principal{UserID: 44, MandateID: "mandate-1", Governed: true},
		reachable: []int64{7}, credentials: []string{"ref-b", "ref-a", "ref-a"},
	}
	policy, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	bound := policy.GetPrincipalBound()
	if !bound.GetCredentialsBounded() || !slices.Equal(bound.GetCredentialReferenceIds(), []string{"ref-a", "ref-b"}) {
		t.Fatalf("bound = %v", bound)
	}
	if stub.hiddenFor != 44 {
		t.Fatalf("a mandate acts for its subject user: hidden asked for %d", stub.hiddenFor)
	}
}

// A governed run whose use grants are gone is bounded to no credential at
// all: a credential bound with an empty list, not an absent one.
func TestAGovernedRunWithNoGrantsHasAnEmptyCredentialBound(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 44, Governed: true}, reachable: []int64{7}}
	policy, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if bound := policy.GetPrincipalBound(); !bound.GetCredentialsBounded() || len(bound.GetCredentialReferenceIds()) != 0 {
		t.Fatalf("bound = %v", bound)
	}
}

// The switch is ON by default (S6), so a saved section that does not say
// otherwise is bounded; a saved false turns it off and nothing is read or sent.
func TestTheBoundIsOnByDefaultAndASavedFalseTurnsItOff(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: []int64{9}, hidden: []int64{9}}
	policy, err := boundedResolver(t, savedDefaults(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil || policy.GetPrincipalBound() == nil || stub.reads == 0 {
		t.Fatalf("default: policy = %v, err = %v, reads = %d", policy, err, stub.reads)
	}

	off := savedDefaults()
	off.PrincipalBound = false
	stub = &principalStub{principal: Principal{UserID: 31}, reachable: []int64{9}, hidden: []int64{9}}
	policy, err = boundedResolver(t, off, stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil {
		t.Fatalf("policy = %v, err = %v", policy, err)
	}
	if policy.GetPrincipalBound() != nil || stub.reads != 0 {
		t.Fatalf("bound = %v, reads = %d; want none while the switch is off", policy.GetPrincipalBound(), stub.reads)
	}
	// And the saved defaults are byte for byte what they were.
	plain, _ := resolver(t, off, agentInput(t, `{"id":5}`), nil)
	want, err := plain.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, policy) {
		t.Fatalf("policy = %v, want %v", policy, want)
	}
}

// A deployment that never saved the section is bounded too (S6): the policy
// carries the compiled-in defaults with the bound, and no data class is read.
func TestAnUnsavedSectionIsBoundedByDefault(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: []int64{7, 9}, hidden: []int64{9}}
	r, classes := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5,"version_id":9,"version_details":{"tools":[
		{"id":7,"type":"jira"}]}}`), map[[2]int64][]string{{17, 5}: {"pii"}})
	policy, err := r.WithPrincipalBound(stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil {
		t.Fatalf("policy = %v, err = %v", policy, err)
	}
	if policy.GetMode() != runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_ENFORCE || policy.GetMaxDepth() != 4 {
		t.Fatalf("defaults: %v", policy)
	}
	if got := toolkitIDs(policy.GetPrincipalBound()); len(got) != 1 || got[0] != 7 {
		t.Fatalf("bound toolkits = %v, want [7]", got)
	}
	if classes.reads != 0 || len(policy.GetRootDataClasses()) != 0 {
		t.Fatalf("an unsaved section reads no registry: reads = %d, classes = %v", classes.reads, policy.GetRootDataClasses())
	}
}

// A closure over the limits refuses the dispatch with the typed error and
// is never truncated; a source that cannot be read is an error to retry.
func TestAnOversizeClosureIsRefusedNotTruncated(t *testing.T) {
	t.Parallel()
	ids := make([]int64, MaxPrincipalToolkits+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	stub := &principalStub{principal: Principal{UserID: 31}, reachable: ids}
	_, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if !errors.Is(err, ErrPrincipalBoundTooLarge) {
		t.Fatalf("err = %v, want ErrPrincipalBoundTooLarge", err)
	}
	failing := &principalStub{err: errors.New("down")}
	if _, err := boundedResolver(t, boundedSettings(), failing).WorkerDelegationPolicy(context.Background(), agentCommand()); err == nil {
		t.Fatal("an unreadable source must not send an unbounded policy")
	}
}

func TestPrincipalBoundIsSortedAndBounded(t *testing.T) {
	t.Parallel()
	bound, err := PrincipalBound([]int64{3, 1, 3, 2}, true, []string{"b", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(toolkitIDs(bound), []uint64{1, 2, 3}) || !slices.Equal(bound.GetCredentialReferenceIds(), []string{"a", "b"}) {
		t.Fatalf("bound = %v", bound)
	}
	if _, err := PrincipalBound(nil, true, []string{string(make([]byte, MaxCredentialIDBytes+1))}); !errors.Is(err, ErrPrincipalBoundTooLarge) {
		t.Fatalf("a long reference: %v", err)
	}
	if _, err := PrincipalBound([]int64{3, 0}, false, nil); err == nil {
		t.Fatal("an invalid toolkit id must fail the dispatch, not become a silent denial")
	}
}

// Seeds tolerate the two spellings of an id; an input that cannot be decoded
// is an error, never an empty set read as "nothing allowed".
func TestSeedsFromInputToleratesStoredSpellings(t *testing.T) {
	t.Parallel()
	if _, err := SeedsFromInput([]byte("not a protobuf")); !errors.Is(err, ErrPrincipalInputUnreadable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := SeedsFromInput(agentInput(t, `{"id":5,"version_details":{"tools":{"not":"an array"}}}`)); !errors.Is(err, ErrPrincipalInputUnreadable) {
		t.Fatalf("a tools value that is no array: %v", err)
	}
	seeds, err := SeedsFromInput(agentInput(t, `{"id":"5","version_id":9,"version_details":{"tools":[
		{"id":7,"type":"jira"},{"id":"x","type":"github"},
		{"type":"application","settings":{"application_id":6,"version_id":"11"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seeds.ToolkitIDs, []int64{7}) {
		t.Fatalf("toolkits = %v", seeds.ToolkitIDs)
	}
	want := []applicationcomposition.Identity{{ApplicationID: 5, VersionID: 9}, {ApplicationID: 6, VersionID: 11}}
	if !slices.Equal(seeds.Versions, want) {
		t.Fatalf("versions = %v, want %v", seeds.Versions, want)
	}
}

// With the switch on, a run that cannot be bounded is not dispatched, and the
// references admission froze count as usable.
func TestARunThatCannotBeBoundedIsNotDispatched(t *testing.T) {
	t.Parallel()
	stub := &principalStub{principal: Principal{UserID: 31}}
	command := agentCommand()
	command.ResourceProjectId = "not-a-project"
	_, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), command)
	if !errors.Is(err, ErrPrincipalProjectUnknown) {
		t.Fatalf("err = %v, want ErrPrincipalProjectUnknown", err)
	}
}

func TestFrozenCredentialReferencesAreUsable(t *testing.T) {
	t.Parallel()
	stub := &principalStub{
		principal: Principal{UserID: 44, Governed: true, FrozenCredentialReferences: []string{"ref-frozen"}},
		reachable: []int64{7}, credentials: []string{"ref-granted"},
	}
	policy, err := boundedResolver(t, boundedSettings(), stub).WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.GetPrincipalBound().GetCredentialReferenceIds(); !slices.Equal(got, []string{"ref-frozen", "ref-granted"}) {
		t.Fatalf("references = %v", got)
	}
}
