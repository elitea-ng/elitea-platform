package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

func envOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		enabled bool
		wantErr bool
	}{
		{name: "unset is off", env: map[string]string{}},
		{name: "blank is off", env: map[string]string{EnvEnabled: "  "}},
		{name: "false", env: map[string]string{EnvEnabled: "false"}},
		{name: "true with revision", env: map[string]string{EnvEnabled: "true", EnvPolicyRevision: "2026-10-r1"}, enabled: true},
		{name: "trimmed true", env: map[string]string{EnvEnabled: " true ", EnvPolicyRevision: "r1"}, enabled: true},
		{name: "upper case refused", env: map[string]string{EnvEnabled: " TRUE "}, wantErr: true},
		{name: "yes refused", env: map[string]string{EnvEnabled: "yes"}, wantErr: true},
		{name: "one refused", env: map[string]string{EnvEnabled: "1"}, wantErr: true},
		{name: "true without revision refused", env: map[string]string{EnvEnabled: "true"}, wantErr: true},
		{name: "malformed revision refused", env: map[string]string{EnvEnabled: "true", EnvPolicyRevision: "Bad Rev"}, wantErr: true},
		{name: "revision ignored while off", env: map[string]string{EnvEnabled: "false", EnvPolicyRevision: "Bad Rev"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ConfigFromEnv(envOf(tc.env))
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("err = %v, want ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Enabled != tc.enabled {
				t.Fatalf("enabled = %v, want %v", cfg.Enabled, tc.enabled)
			}
			if !cfg.Enabled && cfg.ActivePolicyRevisionID != "" {
				t.Fatal("a disabled config must not carry a revision")
			}
		})
	}
}

const validPolicy = `{"schema":"elitea.credential-policy.v1","allowed_identity_modes":["runtime_domain_binding","dedicated_agent"],"vault_required_credential_classes":[],"max_mandate_ttl_seconds":86400,"audit_backlog_alert_threshold":10000}`

func TestParsePolicyDocumentIsStrictAndCanonical(t *testing.T) {
	doc, canonical, err := ParsePolicyDocument([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.AllowsIdentityMode(agentidentity.IdentityModeDedicatedAgent) || doc.AllowsIdentityMode(agentidentity.IdentityModeEliteaNative) {
		t.Fatal("allow-list not read")
	}
	// Order of list items does not change the canonical bytes or digest.
	reordered := strings.Replace(validPolicy, `["runtime_domain_binding","dedicated_agent"]`, `["dedicated_agent","runtime_domain_binding"]`, 1)
	_, again, err := ParsePolicyDocument([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(canonical) != Digest(again) {
		t.Fatal("equal parameters must give equal digests")
	}

	for name, raw := range map[string]string{
		"unknown field":   strings.Replace(validPolicy, `"schema"`, `"token":"x","schema"`, 1),
		"wrong schema":    strings.Replace(validPolicy, "credential-policy.v1", "credential-policy.v2", 1),
		"unknown mode":    strings.Replace(validPolicy, "dedicated_agent", "root", 1),
		"duplicate mode":  strings.Replace(validPolicy, `"runtime_domain_binding","dedicated_agent"`, `"dedicated_agent","dedicated_agent"`, 1),
		"zero mandate":    strings.Replace(validPolicy, "86400", "0", 1),
		"huge mandate":    strings.Replace(validPolicy, "86400", "99999999999", 1),
		"zero audit":      strings.Replace(validPolicy, "10000", "0", 1),
		"trailing":        validPolicy + `{}`,
		"not json":        `{`,
		"empty":           ``,
		"wrong list type": strings.Replace(validPolicy, `"vault_required_credential_classes":[]`, `"vault_required_credential_classes":"all"`, 1),
	} {
		if _, _, err := ParsePolicyDocument([]byte(raw)); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: err = %v, want ErrInvalidPolicy", name, err)
		}
	}
}

type fakeRevisions map[string]PolicyRevision

func (f fakeRevisions) Revision(_ context.Context, id string) (PolicyRevision, error) {
	revision, ok := f[id]
	if !ok {
		return PolicyRevision{}, ErrRevisionNotFound
	}
	return revision, nil
}

func (f fakeRevisions) PutRevision(context.Context, PolicyRevision) error { return nil }

type fakeProbe struct {
	found bool
	err   error
	calls int
}

func (f *fakeProbe) AnyGovernedPin(context.Context) (bool, error) {
	f.calls++
	return f.found, f.err
}

type fakePrerequisite struct{ err error }

func (fakePrerequisite) Name() string                                  { return "fake" }
func (f fakePrerequisite) Check(context.Context, PolicyRevision) error { return f.err }

func revisionFrom(t *testing.T, id, raw string) PolicyRevision {
	t.Helper()
	doc, canonical, err := ParsePolicyDocument([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return PolicyRevision{ID: id, Digest: Digest(canonical), Document: doc, Canonical: canonical, CreatedBy: "operator"}
}

func TestBoot(t *testing.T) {
	ctx := context.Background()
	good := revisionFrom(t, "r1", validPolicy)
	noModes := revisionFrom(t, "r0", strings.Replace(validPolicy, `["runtime_domain_binding","dedicated_agent"]`, `[]`, 1))
	tampered := good
	tampered.ID = "rt"
	tampered.Canonical = []byte(strings.Replace(string(good.Canonical), "86400", "86401", 1))
	store := fakeRevisions{"r1": good, "r0": noModes, "rt": tampered}

	t.Run("off is legacy and probes once", func(t *testing.T) {
		probe := &fakeProbe{found: true}
		admission, _, err := Boot(ctx, Config{}, nil, probe, []Prerequisite{fakePrerequisite{err: errors.New("never run")}})
		if err != nil || admission.Governed() || !admission.ContinuationGuard() || probe.calls != 1 {
			t.Fatalf("admission=%+v err=%v calls=%d", admission, err, probe.calls)
		}
		if err := admission.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("off without governed pins has no guard", func(t *testing.T) {
		admission, _, err := Boot(ctx, Config{}, nil, &fakeProbe{}, nil)
		if err != nil || admission.ContinuationGuard() {
			t.Fatalf("admission=%+v err=%v", admission, err)
		}
	})
	t.Run("off probe failure refuses", func(t *testing.T) {
		if _, _, err := Boot(ctx, Config{}, nil, &fakeProbe{err: errors.New("db down")}, nil); !errors.Is(err, ErrBootRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	on := func(id string) Config { return Config{Enabled: true, ActivePolicyRevisionID: id} }
	t.Run("on pins the revision", func(t *testing.T) {
		admission, revision, err := Boot(ctx, on("r1"), store, nil, []Prerequisite{fakePrerequisite{}})
		if err != nil || !admission.Governed() || admission.RevisionID() != "r1" || admission.Digest() != good.Digest || revision.ID != "r1" {
			t.Fatalf("admission=%+v err=%v", admission, err)
		}
		if err := admission.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	for name, tc := range map[string]struct {
		cfg    Config
		prereq []Prerequisite
	}{
		"missing revision id":  {cfg: Config{Enabled: true}},
		"unknown revision":     {cfg: on("r9")},
		"digest mismatch":      {cfg: on("rt")},
		"no identity mode":     {cfg: on("r0")},
		"failed prerequisite":  {cfg: on("r1"), prereq: []Prerequisite{fakePrerequisite{err: errors.New("vault down")}}},
		"nil prerequisite":     {cfg: on("r1"), prereq: []Prerequisite{nil}},
		"missing store passes": {cfg: on("r1")},
	} {
		t.Run(name, func(t *testing.T) {
			revisions := RevisionStore(store)
			if name == "missing store passes" {
				revisions = nil
			}
			if _, _, err := Boot(ctx, tc.cfg, revisions, nil, tc.prereq); err == nil {
				t.Fatal("boot must refuse")
			}
		})
	}
}

func TestAdmissionZeroValueIsInvalid(t *testing.T) {
	if err := (Admission{}).Validate(); err == nil {
		t.Fatal("the zero admission must be refused so a forgotten wiring fails")
	}
}

// TestProfileIsReadFromTheEnvironmentOnly is the negative check of #1111:
// nothing outside this package and the composition root names the flag, so
// no request, header, body or platform_config row can choose a profile.
func TestProfileIsReadFromTheEnvironmentOnly(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(self), "..", "..", "..")
	allowed := map[string]bool{
		filepath.Join("internal", "credentialgovernance", "profile"): true,
		filepath.Join("cmd", "elitea-main"):                          true,
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[filepath.Dir(rel)] {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, needle := range []string{EnvEnabled, EnvPolicyRevision, "profile.ConfigFromEnv", "profile.EnvEnabled"} {
			if strings.Contains(string(body), needle) {
				t.Errorf("%s names %s: the credential profile is chosen only at the composition root", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The reconcile parameters (#1154) are optional: absent keeps the defaults
// and the canonical bytes of a revision written before they existed.
func TestReconcileParametersAreOptionalAndBounded(t *testing.T) {
	doc, canonical, err := ParsePolicyDocument([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if doc.ReconcileInterval() != DefaultReconcileInterval || doc.ReconcileOutageFailures() != DefaultReconcileOutageThreshold {
		t.Fatal("absent reconcile parameters must read as the defaults")
	}
	if strings.Contains(string(canonical), "reconcile") {
		t.Fatalf("absent reconcile parameters must not change the canonical bytes: %s", canonical)
	}
	set := strings.Replace(validPolicy, `}`, `,"reconcile_interval_seconds":300,"reconcile_outage_threshold":5}`, 1)
	doc, _, err = ParsePolicyDocument([]byte(set))
	if err != nil {
		t.Fatal(err)
	}
	if doc.ReconcileInterval() != 5*time.Minute || doc.ReconcileOutageFailures() != 5 {
		t.Fatalf("reconcile parameters not read: %v %d", doc.ReconcileInterval(), doc.ReconcileOutageFailures())
	}
	for name, extra := range map[string]string{
		"interval too short": `,"reconcile_interval_seconds":10}`,
		"interval too long":  `,"reconcile_interval_seconds":999999}`,
		"negative interval":  `,"reconcile_interval_seconds":-1}`,
		"negative threshold": `,"reconcile_outage_threshold":-1}`,
		"huge threshold":     `,"reconcile_outage_threshold":1000}`,
	} {
		raw := strings.Replace(validPolicy, `}`, extra, 1)
		if _, _, err := ParsePolicyDocument([]byte(raw)); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: err = %v, want ErrInvalidPolicy", name, err)
		}
	}
}
