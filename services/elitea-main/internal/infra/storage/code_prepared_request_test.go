package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const codePreparedFixtureDirectory = "../../../../../libs/proto/elitea/runtime/v1/"

func codePreparedFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(codePreparedFixtureDirectory + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCodePreparedCrossLanguageRevisionAndRawByteIdentity(t *testing.T) {
	var cases []struct {
		File           string `json:"file"`
		SHA256         string `json:"sha256"`
		Fingerprint    string `json:"fingerprint"`
		PreSHA256      string `json:"pre_workspace_sha256"`
		PreFingerprint string `json:"pre_workspace_fingerprint"`
	}
	if json.Unmarshal(codePreparedFixture(t, "code_prepared_workspace_cases_v1.json"), &cases) != nil {
		t.Fatal("invalid source fixture")
	}
	for _, fixture := range cases {
		t.Run(fixture.File, func(t *testing.T) {
			raw := codePreparedFixture(t, fixture.File)
			parsed, err := ParseCodePreparedRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.PreparedSHA256 != fixture.SHA256 || parsed.Fingerprint != fixture.Fingerprint || parsed.PreWorkspaceSHA256 != fixture.PreSHA256 || parsed.PreWorkspaceFingerprint != fixture.PreFingerprint {
				t.Fatal("exact Rust bytes or pre-workspace reconstruction changed")
			}
			base, err := ParseCodePreparedRequest(parsed.PreWorkspaceBytes)
			if err != nil {
				t.Fatal(err)
			}
			if base.Workspace != nil || base.Fingerprint != fixture.PreFingerprint {
				t.Fatal("pre-workspace retains repository authority")
			}
			var original struct {
				Bundle string `json:"dependency_bundle_sha256"`
			}
			if json.Unmarshal(raw, &original) != nil || parsed.DependencyBundleSHA256 != original.Bundle || base.DependencyBundleSHA256 != original.Bundle {
				t.Fatal("validated immutable dependency bundle changed during workspace removal")
			}
			if parsed.Broker != nil && (base.Revision != 5 || base.Broker == nil || *base.Broker != *parsed.Broker) {
				t.Fatal("workspace removal changes broker authority")
			}
			if !bytes.Contains(parsed.Input, []byte("9007199254740993")) || !bytes.Contains(parsed.Input, []byte("λ\u2028\u2029")) {
				t.Fatal("selected input lost numeric or Unicode bytes")
			}
		})
	}
}

func TestCodePreparedRejectsSmugglingNullUnknownAndChangedNativeSource(t *testing.T) {
	workspace := codePreparedFixture(t, "code_prepared_workspace_only_v4.json")
	broker := codePreparedFixture(t, "code_prepared_workspace_broker_v5.json")
	native := codePreparedFixture(t, "code_prepared_workspace_native_v3.json")
	var cases [][]byte
	for _, revision := range []string{"1", "2", "3", "5"} {
		cases = append(cases, bytes.Replace(workspace, []byte(`"revision":4,`), []byte(`"revision":`+revision+`,`), 1))
	}
	cases = append(cases,
		bytes.Replace(broker, []byte(`"revision":5,`), []byte(`"revision":1,`), 1),
		bytes.Replace(workspace, []byte(`"workspace":{`), []byte(`"unknown":true,"workspace":{`), 1),
		bytes.Replace(workspace, []byte(`"revision":4,`), []byte(`"revision":4,"revision":4,`), 1),
		bytes.Replace(native, []byte(`"source":"return 7;"`), []byte(`"source":"return 8;"`), 1),
		bytes.Replace(native, []byte(`"kind":"deno"`), []byte(`"kind":"cargo"`), 1),
		bytes.Replace(workspace, []byte(`"large":9007199254740993,`), []byte(`"large":9007199254740993,"large":9007199254740993,`), 1),
		append(bytes.Clone(workspace), '\n'),
	)
	var record map[string]json.RawMessage
	if json.Unmarshal(workspace, &record) != nil {
		t.Fatal("fixture")
	}
	record["workspace"] = json.RawMessage("null")
	null, _ := json.Marshal(record)
	cases = append(cases, null)
	for index, raw := range cases {
		if _, err := ParseCodePreparedRequest(raw); err == nil {
			t.Fatalf("accepted invalid schema case %d", index)
		}
	}
}

func TestCodePreparedChecksStructureBeforeCanonicalization(t *testing.T) {
	raw := codePreparedFixture(t, "code_prepared_workspace_legacy_v1.json")
	needle := []byte(`"input":`)
	start := bytes.Index(raw, needle) + len(needle)
	end := bytes.Index(raw[start:], []byte(`,"image_digest":`)) + start
	if start < len(needle) || end < start {
		t.Fatal("fixture input boundary")
	}
	deep := `{"nested":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}"
	changed := append(bytes.Clone(raw[:start]), []byte(deep)...)
	changed = append(changed, raw[end:]...)
	if _, err := ParseCodePreparedRequest(changed); err == nil {
		t.Fatal("accepted selected input beyond depth64")
	}
	invalid := bytes.Replace(raw, []byte(`"source":"`), []byte(`"source":"\ud800`), 1)
	if _, err := ParseCodePreparedRequest(invalid); err == nil {
		t.Fatal("accepted unpaired surrogate not admitted by Rust serde")
	}
	if _, err := ParseCodePreparedRequest(bytes.Repeat([]byte(" "), 1024*1024+1)); err == nil {
		t.Fatal("accepted unbounded request")
	}
}

func TestCodePreparedLegacyRoundtripDoesNotAddOptionalAuthority(t *testing.T) {
	for _, name := range []string{"code_prepared_workspace_legacy_v1.json", "code_prepared_workspace_python_v2.json", "code_prepared_workspace_native_v3.json"} {
		raw := codePreparedFixture(t, name)
		parsed, err := ParseCodePreparedRequest(raw)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Workspace != nil || parsed.Broker != nil || !bytes.Equal(parsed.PreWorkspaceBytes, raw) || parsed.PreWorkspaceFingerprint != parsed.Fingerprint {
			t.Fatal("omitted fields changed legacy request")
		}
	}
}
