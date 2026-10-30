package workercontentcontrols

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type settingsStub struct {
	settings platformconfig.ContentControlsSettings
	err      error
}

func (s settingsStub) Get(context.Context) (platformconfig.ContentControlsSettings, error) {
	return s.settings, s.err
}

type projectStub struct {
	setting ProjectSetting
	err     error
	reads   int
}

func (s *projectStub) AIDisclosureProjectSetting(context.Context, int64) (ProjectSetting, error) {
	s.reads++
	return s.setting, s.err
}

type inputStub map[string][]byte

func (s inputStub) InputEntries(_ context.Context, _ string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, id := range ids {
		if content, ok := s[id]; ok {
			out[id] = content
		}
	}
	return out, nil
}

type tierStub struct {
	tier  string
	reads int
}

func (s *tierStub) AgentRiskTier(context.Context, int64, int64) (string, error) {
	s.reads++
	return s.tier, nil
}

func agentCommand(t *testing.T, preamble ...*runtimev1.PlatformPreambleSnapshotV1) (*runtimev1.WorkerCommandV1, inputStub) {
	t.Helper()
	input := &runtimev1.AgentExecutionInputV1{Application: []byte(`{"id":5}`)}
	if len(preamble) > 0 {
		input.PlatformPreamble = preamble[0]
	}
	content, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.WorkerCommandV1{
		ResourceProjectId: "17",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "request"},
		},
	}, inputStub{"request": content}
}

// The rollout gate: nothing saved answers nil and reads nothing else.
func TestResolverIsAbsentUntilSaved(t *testing.T) {
	t.Parallel()
	command, inputs := agentCommand(t)
	projects, tiers := &projectStub{}, &tierStub{tier: "high"}
	resolver, err := NewResolver(settingsStub{settings: platformconfig.DefaultContentControlsSettings()}, projects, inputs, tiers)
	if err != nil {
		t.Fatal(err)
	}
	controls, err := resolver.WorkerContentControls(context.Background(), command)
	if err != nil || controls != nil || projects.reads != 0 || tiers.reads != 0 {
		t.Fatalf("controls = %v, err = %v, reads = %d/%d", controls, err, projects.reads, tiers.reads)
	}
	// A command that is not an agent execution never carries them.
	stored := platformconfig.DefaultContentControlsSettings()
	stored.Stored = true
	resolver, _ = NewResolver(settingsStub{settings: stored}, projects, inputs, tiers)
	if controls, err := resolver.WorkerContentControls(context.Background(), &runtimev1.WorkerCommandV1{}); controls != nil || err != nil {
		t.Fatalf("controls = %v, err = %v", controls, err)
	}
}

func TestResolverCarriesTheProjectTextAndTheTierRule(t *testing.T) {
	t.Parallel()
	settings := platformconfig.ContentControlsSettingsFromValues(
		platformconfig.Values{"marker_text": "Drafted with ACME AI", "opt_out_toolkit_types": []any{"slack"}},
		platformconfig.Values{"text": "You work for ACME."},
		platformconfig.Values{})
	for name, test := range map[string]struct {
		tier    string
		project ProjectSetting
		text    string
		optOuts []string
		rule    bool
	}{
		"platform text at high": {"high", ProjectSetting{}, "Drafted with ACME AI", []string{}, true},
		"project text at low":   {"", ProjectSetting{MarkerText: "Team bot"}, "Team bot", []string{}, false},
		"only allowed opt-outs": {"critical", ProjectSetting{OptOutToolkitTypes: []string{"Slack", "jira", "slack"}}, "Drafted with ACME AI", []string{"slack"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			command, inputs := agentCommand(t, acmePreamble())
			resolver, err := NewResolver(settingsStub{settings: settings}, &projectStub{setting: test.project}, inputs, &tierStub{tier: test.tier})
			if err != nil {
				t.Fatal(err)
			}
			controls, err := resolver.WorkerContentControls(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if controls.GetDisclosureText() != test.text || !slices.Equal(controls.GetDisclosureOptOutToolkitTypes(), test.optOuts) ||
				controls.GetUntrustedReadThenAct() != test.rule || controls.GetPlatformPreambleSha256() != acmePreamble().GetSha256() ||
				controls.GetPlatformPreambleRevision() != 4 {
				t.Fatalf("controls = %v", controls)
			}
		})
	}
}

func TestControlsDefaultTextAndTierFreeRule(t *testing.T) {
	t.Parallel()
	settings := platformconfig.ContentControlsSettingsFromValues(platformconfig.Values{},
		platformconfig.Values{}, platformconfig.Values{"untrusted_read_then_act_tiers": []any{"low", "medium", "high", "critical"}})
	if !settings.Stored || needsTier(settings) {
		t.Fatalf("settings = %+v", settings)
	}
	controls := Controls(settings, ProjectSetting{}, agentlifecycle.TierLow, nil)
	if controls.GetDisclosureText() != "Generated with Elitea AI" || !controls.GetUntrustedReadThenAct() {
		t.Fatalf("controls = %v", controls)
	}
	off := platformconfig.ContentControlsSettingsFromValues(platformconfig.Values{}, platformconfig.Values{},
		platformconfig.Values{"untrusted_read_then_act_tiers": []any{}})
	if Controls(off, ProjectSetting{}, agentlifecycle.TierCritical, nil).GetUntrustedReadThenAct() {
		t.Fatal("an empty tier list must turn the rule off")
	}
}

func TestResolverFailsClosed(t *testing.T) {
	t.Parallel()
	command, inputs := agentCommand(t)
	stored := platformconfig.DefaultContentControlsSettings()
	stored.Stored = true
	failure := errors.New("down")
	for name, resolver := range map[string]*Resolver{
		"settings": {settings: settingsStub{err: failure}, projects: &projectStub{}, inputs: inputs, tiers: &tierStub{}},
		"project":  {settings: settingsStub{settings: stored}, projects: &projectStub{err: failure}, inputs: inputs, tiers: &tierStub{}},
		"tier":     {settings: settingsStub{settings: stored}, projects: &projectStub{}, inputs: inputs, tiers: &tierStub{tier: "bogus"}},
	} {
		if _, err := resolver.WorkerContentControls(context.Background(), command); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := NewResolver(nil, nil, nil, nil); err == nil {
		t.Error("want an error for missing readers")
	}
}

func acmePreamble() *runtimev1.PlatformPreambleSnapshotV1 {
	const text = "You work for ACME."
	return &runtimev1.PlatformPreambleSnapshotV1{Revision: 4, Sha256: agentexecution.PlatformPreambleDigest(text), Text: text}
}

// The preamble text never goes on the command: only its revision and
// digest. An input that carries a preamble sends the controls also when this
// replica's cache has not seen the save yet.
func TestResolverNamesTheInputsPreambleByDigestOnly(t *testing.T) {
	t.Parallel()
	command, inputs := agentCommand(t, acmePreamble())
	resolver, err := NewResolver(settingsStub{settings: platformconfig.DefaultContentControlsSettings()}, &projectStub{}, inputs, &tierStub{tier: "high"})
	if err != nil {
		t.Fatal(err)
	}
	controls, err := resolver.WorkerContentControls(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(controls)
	if err != nil {
		t.Fatal(err)
	}
	if controls.GetPlatformPreambleSha256() != acmePreamble().GetSha256() || controls.GetPlatformPreambleRevision() != 4 ||
		strings.Contains(string(encoded), "ACME") || !controls.GetUntrustedReadThenAct() {
		t.Fatalf("controls = %v", controls)
	}
}
