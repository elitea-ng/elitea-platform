package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

func debugFixture(t *testing.T) (CodeDebugAdmission, []byte) {
	t.Helper()
	source := "# λ\r\nprint('quote\\ntext')\n"
	input := json.RawMessage(`{"nested":[1,null,{"unknown":"value"}],"large":9007199254740993}`)
	config := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":` + string(mustDebugJSON(t, source)) + `},"input":[],"output":[],"structured_output":false,"debug":true,"transition":null}`
	snapshot := []byte(`{"schema_version":"elitea.runtime.code-debug-snapshot.v1","language":"python","source":` + string(mustDebugJSON(t, source)) + `,"selected_input":` + string(input) + `}`)
	a := CodeDebugAdmission{OriginalVisit: code.OriginalVisitRef{VisitID: strings.Repeat("1", 64), Revision: 1, DigestSHA256: strings.Repeat("2", 64)}, Attempt: 1, SchemaVersion: CodeDebugAdmissionSchema, NodeID: "run", GraphThreadID: "thread", GraphStep: "2", DefinitionSHA256: strings.Repeat("a", 64), YAMLSHA256: strings.Repeat("b", 64), ConfigurationJSON: config, RequestSHA256: strings.Repeat("c", 64), SourceSHA256: CodeDebugSHA256([]byte(source)), InputSHA256: CodeDebugSHA256(input), SnapshotSHA256: CodeDebugSHA256(snapshot), ByteLength: int64(len(snapshot))}
	// Opaque logical activation fixture. Only the real Started authority owns
	// production minting; Main must not derive it from these selected values.
	a.ActivationID = strings.Repeat("d", 64)
	return a, snapshot
}
func mustDebugJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestCodeDebugExactSnapshotAndIntentShape(t *testing.T) {
	a, raw := debugFixture(t)
	if ValidateCodeDebugAdmission(a) != nil || ValidateCodeDebugSnapshot(raw, a, "python") != nil {
		t.Fatal("exact snapshot refused")
	}
	for _, mutate := range []func(*CodeDebugAdmission){func(a *CodeDebugAdmission) { a.GraphStep = "02" }, func(a *CodeDebugAdmission) { a.NodeID = "other" }, func(a *CodeDebugAdmission) { a.InputSHA256 = strings.Repeat("d", 64) }, func(a *CodeDebugAdmission) { a.ByteLength++ }} {
		changed := a
		mutate(&changed)
		if ValidateCodeDebugAdmission(changed) == nil && ValidateCodeDebugSnapshot(raw, changed, "python") == nil {
			t.Fatal("changed identity/content accepted")
		}
	}
	for _, extra := range []string{`,"grant":"secret"`, `,"source":"duplicate"`, `,"actor_id":9`} {
		changed := append(append([]byte{}, raw[:len(raw)-1]...), []byte(extra+"}")...)
		b := a
		b.ByteLength = int64(len(changed))
		b.SnapshotSHA256 = CodeDebugSHA256(changed)
		if ValidateCodeDebugSnapshot(changed, b, "python") == nil {
			t.Fatal("foreign or duplicate snapshot field accepted")
		}
	}
	if ValidateCodeDebugSnapshot(raw, a, "rust") == nil {
		t.Fatal("wrong language accepted")
	}
}
func TestCodeDebugPolicyRequiresOriginalSavedDebugAndLiteralSource(t *testing.T) {
	a, _ := debugFixture(t)
	var config map[string]any
	if err := json.Unmarshal([]byte(a.ConfigurationJSON), &config); err != nil {
		t.Fatal(err)
	}
	yamlSource := "state: {input: str}\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    debug: true\n    code: " + string(mustDebugJSON(t, config["code"].(map[string]any)["value"])) + "\n"
	policies, err := CodeDebugPolicies(yamlSource)
	if err != nil {
		t.Fatal(err)
	}
	if language, err := MatchCodeDebugPolicy(policies["run"], a); err != nil || language != "python" {
		t.Fatal("original saved policy refused")
	}
	for _, document := range []string{strings.Replace(yamlSource, "debug: true", "debug: false", 1), strings.Replace(yamlSource, "    debug: true\n", "", 1)} {
		policies, err := CodeDebugPolicies(document)
		if err != nil || len(policies) != 0 {
			t.Fatal("off or omitted debug entered policy cache")
		}
	}
	changed := a
	changed.SourceSHA256 = strings.Repeat("e", 64)
	if _, err := MatchCodeDebugPolicy(policies["run"], changed); err == nil {
		t.Fatal("changed literal source accepted")
	}
	for _, document := range []string{yamlSource + "    debug: true\n", strings.Replace(yamlSource, "    debug: true", "    debug: 'true'", 1)} {
		policies, err := CodeDebugPolicies(document)
		if err == nil && len(policies) > 0 {
			t.Fatal("duplicate or mistyped declaration accepted")
		}
	}
}

type debugAuthorityFunc func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error)
type debugRepositoryStub struct {
	admission         CodeDebugAdmission
	staged, committed int
	raw               []byte
	authority         debugAuthorityFunc
}

func (r *debugRepositoryStub) authorized(ctx context.Context, c ContentClaim, a CodeDebugAdmission) (CodeDebugAuthorization, error) {
	if r.authority != nil {
		return r.authority(ctx, c, a)
	}
	return CodeDebugAuthorization{ProjectID: 7, ActorID: 11, Language: "python"}, nil
}
func (r *debugRepositoryStub) StageCodeDebug(ctx context.Context, c ContentClaim, a CodeDebugAdmission) error {
	if _, err := r.authorized(ctx, c, a); err != nil {
		return err
	}
	r.admission = a
	r.staged++
	return nil
}
func (r *debugRepositoryStub) PrepareCodeDebugUpload(ctx context.Context, c ContentClaim, _ string) (CodeDebugUpload, error) {
	auth, err := r.authorized(ctx, c, r.admission)
	if err != nil {
		return CodeDebugUpload{}, err
	}
	return CodeDebugUpload{Admission: r.admission, Authorization: auth, ObjectKey: strings.Repeat("3", 64) + ".json", State: "staging"}, nil
}
func (r *debugRepositoryStub) CommitCodeDebugUpload(ctx context.Context, c ContentClaim, u CodeDebugUpload, raw []byte) (CodeDebugArtifactReference, error) {
	if _, err := r.authorized(ctx, c, u.Admission); err != nil {
		return CodeDebugArtifactReference{}, err
	}
	r.committed++
	r.raw = append([]byte(nil), raw...)
	return CodeDebugArtifactReference{SHA256: u.Admission.SnapshotSHA256}, nil
}
func TestCodeDebugRefusedAuthorityProducesNoArtifactAndCommitRechecks(t *testing.T) {
	a, raw := debugFixture(t)
	repo := new(debugRepositoryStub)
	checks := 0
	revoked := false
	repo.authority = func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		checks++
		if revoked {
			return CodeDebugAuthorization{}, ErrContentUnauthorized
		}
		return CodeDebugAuthorization{ProjectID: 7, ActorID: 11, Language: "python"}, nil
	}
	service, err := NewRuntimeCodeDebugArtifactService(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Admit(context.Background(), ContentClaim{}, a); err != nil || repo.staged != 1 {
		t.Fatal("original intent did not stage")
	}
	revoked = true
	if _, err = service.Commit(context.Background(), ContentClaim{}, a.OriginalVisit.VisitID, raw); err == nil || repo.committed != 0 {
		t.Fatal("revoked authority committed bytes")
	}
	revoked = false
	if _, err = service.Commit(context.Background(), ContentClaim{}, a.OriginalVisit.VisitID, raw); err != nil || !bytes.Equal(raw, repo.raw) || checks != 4 {
		t.Fatal("exact commit or final authority recheck failed", err, checks)
	}
	upload, err := service.prepareCommit(context.Background(), ContentClaim{}, a.OriginalVisit.VisitID)
	if err != nil {
		t.Fatal(err)
	}
	revoked = true
	if _, err = service.commitAuthorized(context.Background(), ContentClaim{}, upload, raw); err == nil || repo.committed != 1 {
		t.Fatal("revocation during upload published a reference")
	}
}

func TestOriginalSavedCodeDeclarationHasNoDebugEligibilityRequirement(t *testing.T) {
	for _, kind := range []string{"fixed", "variable", "fstring"} {
		text := "source_text"
		if kind == "fstring" {
			text = "{source_text}"
		}
		document := "entry_point: run\nstate: {source_text: str}\nnodes:\n - {id: run, type: code, language: python, code: {type: " + kind + ", value: '" + text + "'}, input: [], output: [], workspace: {toolkit_id: 7, mode: read, include: [src]}}\n"
		policies, err := SavedCodePolicies(document)
		if err != nil {
			t.Fatal(err)
		}
		var semantic map[string]any
		_ = json.Unmarshal(policies["run"], &semantic)
		exact := string(mustDebugJSON(t, semantic))
		declaration, err := MatchOriginalSavedCodeConfiguration(policies["run"], exact)
		if err != nil || declaration.Debug || declaration.Language != "python" || len(declaration.Workspace) == 0 {
			t.Fatalf("kind=%s err=%v", kind, err)
		}
		if !bytes.Contains(declaration.Source, []byte(kind)) || declaration.ConfigurationDigest == [32]byte{} {
			t.Fatal("source declaration or exact digest lost")
		}
		eligible, err := CodeDebugPolicies(document)
		if err != nil || len(eligible) != 0 {
			t.Fatal("generic policy widened debug eligibility")
		}
		semantic["workspace"].(map[string]any)["mode"] = "readwrite"
		if _, err = MatchOriginalSavedCodeConfiguration(policies["run"], string(mustDebugJSON(t, semantic))); err == nil {
			t.Fatal("changed workspace passed original policy")
		}
	}
}

func TestSavedCodePolicyPreservesCatalogRecoveryAndRejectsExtraDocuments(t *testing.T) {
	document := "entry_point: run\nstate: {answer: int}\nnodes:\n - id: run\n   type: code\n   debug: true\n   code: '7'\n   output: [answer]\n   recovery: {retry: {max_attempts: 2}}\n   failure_handler: {error_input: saved_error}\n"
	policies, err := SavedCodePolicies(document)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	_ = json.Unmarshal(policies["run"], &config)
	delete(config, "recovery")
	delete(config, "failure_handler")
	exact := string(mustDebugJSON(t, config))
	declaration, err := MatchOriginalSavedCodeConfiguration(policies["run"], exact)
	if err != nil || len(declaration.Recovery) == 0 || len(declaration.FailureHandler) == 0 {
		t.Fatal("compiler-owned catalog policy lost or blocked Code serde")
	}
	original := append([]byte{}, policies["run"]...)
	if _, err = MatchOriginalSavedCodeConfiguration(policies["run"], exact); err != nil || !bytes.Equal(original, policies["run"]) {
		t.Fatal("policy lookup mutated original")
	}
	config["vendor"] = nil
	if _, err = MatchOriginalSavedCodeConfiguration(policies["run"], string(mustDebugJSON(t, config))); err == nil {
		t.Fatal("unknown configuration field ignored")
	}
	for _, extra := range []string{"---\nother: true\n", "---\n"} {
		if _, err = SavedCodePolicies(document + extra); err == nil {
			t.Fatal("multiple YAML documents accepted")
		}
	}
	pin := CodeDebugSHA256([]byte(document))
	if _, err = OriginalSavedCodePolicy(document, pin, "run"); err != nil {
		t.Fatal(err)
	}
	if _, err = OriginalSavedCodePolicy(document, strings.Repeat("f", 64), "run"); err == nil {
		t.Fatal("wrong owning YAML accepted")
	}
}

func TestCodeDebugOriginalVisitBindsLogicalActivationAndExactSnapshot(t *testing.T) {
	a, _ := debugFixture(t)
	digest := sha256CodeConfiguration(a.ConfigurationJSON)
	v := OriginalCodeVisit{TenantID: "7", ResourceProjectID: 7, ProjectionProjectID: 7, ActorID: 11, CurrentClaimAttempt: 1, LeaseEpoch: 2, Reference: a.OriginalVisit, ExecutionID: "execution-1", OriginalGeneration: 7, ActivationID: a.ActivationID, NodeID: a.NodeID, GraphThread: a.GraphThreadID, Step: 2, Attempt: 1, NodeDigest: hex.EncodeToString(digest[:]), OwningYAMLSHA256: a.YAMLSHA256, PreWorkspacePreparedSHA256: a.RequestSHA256, SourceSHA256: a.SourceSHA256, InputSHA256: a.InputSHA256, Language: "python", Declaration: SavedCodeDeclaration{ConfigurationDigest: digest, Language: "python", Debug: true}}
	v.OwningSourceDefinition = scope.SourceReference{SchemaVersion: scope.SourceSchema, SourceID: strings.Repeat("1", 64), Revision: 1, DigestSHA256: strings.Repeat("2", 64), SourceDefinitionSHA256: strings.Repeat("3", 64), YAMLSHA256: a.YAMLSHA256, ApplicationID: 7, VersionID: 11, Kind: "saved_application"}
	claim := ContentClaim{ExecutionID: "execution-1", Generation: 7}
	if err := ValidateCodeDebugOriginalVisit(claim, a, v); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*OriginalCodeVisit){"code-attempt": func(v *OriginalCodeVisit) { v.Attempt++ }, "logical-activation": func(v *OriginalCodeVisit) { v.ActivationID = strings.Repeat("f", 64) }, "origin-generation": func(v *OriginalCodeVisit) { v.OriginalGeneration++ }, "thread": func(v *OriginalCodeVisit) { v.GraphThread = "other" }, "node": func(v *OriginalCodeVisit) { v.NodeID = "other" }, "step": func(v *OriginalCodeVisit) { v.Step++ }, "input": func(v *OriginalCodeVisit) { v.InputSHA256 = strings.Repeat("f", 64) }, "source": func(v *OriginalCodeVisit) { v.SourceSHA256 = strings.Repeat("f", 64) }, "prepared": func(v *OriginalCodeVisit) { v.PreWorkspacePreparedSHA256 = strings.Repeat("f", 64) }, "saved-debug-off": func(v *OriginalCodeVisit) { v.Declaration.Debug = false }, "current-attempt": func(v *OriginalCodeVisit) { v.CurrentClaimAttempt = 0 }, "ref": func(v *OriginalCodeVisit) { v.Reference.DigestSHA256 = strings.Repeat("f", 64) }} {
		t.Run(name, func(t *testing.T) {
			changed := v
			mutate(&changed)
			if ValidateCodeDebugOriginalVisit(claim, a, changed) == nil {
				t.Fatal("changed original authority accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*OriginalCodeVisit){
		"missing-owning-source":        func(v *OriginalCodeVisit) { v.OwningSourceDefinition = scope.SourceReference{} },
		"malformed-source-fingerprint": func(v *OriginalCodeVisit) { v.OwningSourceDefinition.SourceDefinitionSHA256 = "not-a-hash" },
		"source-revision":              func(v *OriginalCodeVisit) { v.OwningSourceDefinition.Revision++ },
		"source-yaml":                  func(v *OriginalCodeVisit) { v.OwningSourceDefinition.YAMLSHA256 = strings.Repeat("f", 64) },
		"saved-source-without-version": func(v *OriginalCodeVisit) { v.OwningSourceDefinition.VersionID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := v
			mutate(&changed)
			if ValidateCodeDebugOriginalVisit(claim, a, changed) == nil {
				t.Fatal("missing or malformed trusted complete source identity accepted")
			}
		})
	}
	if v.OwningSourceDefinition.SourceDefinitionSHA256 == a.DefinitionSHA256 {
		t.Fatal("fixture failed to distinguish Main source and Rust compiler identity")
	}
	changed := a
	changed.ConfigurationJSON = strings.Replace(a.ConfigurationJSON, "python", "rust", 1)
	if ValidateCodeDebugOriginalVisit(claim, changed, v) == nil {
		t.Fatal("configuration bytes were rebound")
	}
}

func TestCodeDebugNewRetryRequiresItsOwnImmutableVisitAndActualAttempt(t *testing.T) {
	a, _ := debugFixture(t)
	for _, attempt := range []uint16{0, 17} {
		changed := a
		changed.Attempt = attempt
		if ValidateCodeDebugAdmission(changed) == nil {
			t.Fatal("out-of-contract attempt admitted")
		}
	}
	first, _ := json.Marshal(a)
	retry := a
	retry.OriginalVisit.VisitID = strings.Repeat("3", 64)
	retry.OriginalVisit.DigestSHA256 = strings.Repeat("4", 64)
	retry.Attempt = 2
	second, _ := json.Marshal(retry)
	if ValidateCodeDebugAdmission(retry) != nil || bytes.Equal(first, second) || retry.ActivationID != a.ActivationID || retry.RequestSHA256 != a.RequestSHA256 {
		t.Fatal("same logical activation/request obscured distinct retry identity")
	}
	// A writer replacement changes current claim ownership, never immutable visit bytes.
	replacement, _ := json.Marshal(a)
	if !bytes.Equal(first, replacement) {
		t.Fatal("same-visit replacement changed publication identity")
	}
}
