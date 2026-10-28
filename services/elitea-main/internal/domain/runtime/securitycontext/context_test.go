package securitycontext

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func validContext() Context {
	return Context{
		PolicyRevisionID:    "2026-10-r1",
		PolicyDigest:        runtimedomain.SHA256([]byte("policy")),
		TenantProjectID:     42,
		SubjectPrincipalRef: "7",
		AgentBindingID:      "0192f0aa-0000-7000-8000-000000000001",
		AgentBindingEpoch:   3,
		Caller:              Caller{AuthenticationSource: "session"},
		DefinitionDigest:    runtimedomain.SHA256([]byte("definition")),
		ActorChain:          []ActorLink{{Kind: "user", Ref: "7"}},
		IssuedAt:            time.UnixMilli(1_790_000_000_000).UTC(),
	}
}

// governedContext is validContext with the admitted application version and
// the frozen credential references (#1119); toolkit 9 is frozen at its
// digest alone.
func governedContext() Context {
	c := validContext()
	c.ApplicationID, c.ApplicationVersionID = 11, 12
	c.CredentialReferences = []CredentialReference{
		{ToolkitID: 5, CredentialReferenceID: "0192f0aa-0000-7000-8000-0000000000c1", TargetBindingID: "0192f0aa-0000-7000-8000-0000000000d1", Generation: 2, ToolkitDigest: runtimedomain.SHA256([]byte("toolkit-5"))},
		{ToolkitID: 9, ToolkitDigest: runtimedomain.SHA256([]byte("toolkit-9"))},
	}
	return c
}

func TestCredentialReferencesRoundTrip(t *testing.T) {
	encoded, ref, err := governedContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Verify(encoded, ref)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GetApplicationId() != 11 || decoded.GetApplicationVersionId() != 12 {
		t.Fatalf("application = %d/%d", decoded.GetApplicationId(), decoded.GetApplicationVersionId())
	}
	for _, want := range governedContext().CredentialReferences {
		got, ok := CredentialReferenceFor(decoded, want.ToolkitID)
		if !ok || got != want {
			t.Fatalf("toolkit %d = %+v %v", want.ToolkitID, got, ok)
		}
	}
	if _, ok := CredentialReferenceFor(decoded, 6); ok {
		t.Fatal("a toolkit with no frozen reference must answer none")
	}
}

func TestMarshalIsDeterministicAndVerifies(t *testing.T) {
	first, ref, err := validContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	second, again, err := validContext().Marshal("execution-1")
	if err != nil || string(first) != string(second) || ref != again {
		t.Fatalf("marshal is not deterministic: %v", err)
	}
	if ref.ContentID != "elitea.security-context/execution-1" || ref.SchemaRevision != SchemaRevisionV1 || ref.Digest != runtimedomain.SHA256(first) {
		t.Fatalf("reference: %+v", ref)
	}
	decoded, err := Verify(first, ref)
	if err != nil || decoded.GetAgentBindingEpoch() != 3 || decoded.GetProfile() != GovernedProfile {
		t.Fatalf("verify: %+v err=%v", decoded, err)
	}
	back, err := ReferenceFromProto(ref.ReferenceProto())
	if err != nil || back != ref {
		t.Fatalf("reference round trip: %+v err=%v", back, err)
	}
}

func TestZeroReferenceLeavesTheCommandFieldUnset(t *testing.T) {
	if (Reference{}).ReferenceProto() != nil {
		t.Fatal("a legacy command must not carry field 65")
	}
	if ref, err := ReferenceFromProto(nil); err != nil || ref != (Reference{}) {
		t.Fatalf("nil field: %+v %v", ref, err)
	}
}

func TestValidateRefuses(t *testing.T) {
	cases := map[string]func(*Context){
		"no revision":           func(c *Context) { c.PolicyRevisionID = "" },
		"no policy digest":      func(c *Context) { c.PolicyDigest = runtimedomain.Digest{} },
		"no tenant":             func(c *Context) { c.TenantProjectID = 0 },
		"no issued at":          func(c *Context) { c.IssuedAt = time.Time{} },
		"epoch without mandate": func(c *Context) { c.MandateEpoch = 1 },
		"negative epoch":        func(c *Context) { c.AgentBindingEpoch = -1 },
		"binding without digest": func(c *Context) {
			c.DefinitionDigest = runtimedomain.Digest{}
		},
		"long reference": func(c *Context) { c.SubjectPrincipalRef = strings.Repeat("a", 129) },
		"space in ref":   func(c *Context) { c.DelegationID = "a b" },
		"actor chain of nine": func(c *Context) {
			c.ActorChain = nil
			for range MaxActorChain + 1 {
				c.ActorChain = append(c.ActorChain, ActorLink{Kind: "agent", Ref: "x"})
			}
		},
		"empty actor link": func(c *Context) { c.ActorChain = []ActorLink{{Kind: "user"}} },
		"version without application": func(c *Context) { c.ApplicationID = 0 },
		"references out of order": func(c *Context) {
			c.CredentialReferences[0], c.CredentialReferences[1] = c.CredentialReferences[1], c.CredentialReferences[0]
		},
		"reference without a digest": func(c *Context) { c.CredentialReferences[0].ToolkitDigest = runtimedomain.Digest{} },
		"reference without a target": func(c *Context) { c.CredentialReferences[0].TargetBindingID = "" },
		"target without a reference": func(c *Context) { c.CredentialReferences[1].TargetBindingID = "0192f0aa-0000-7000-8000-0000000000d2" },
		"references without a binding": func(c *Context) {
			c.AgentBindingID, c.AgentBindingEpoch, c.DefinitionDigest = "", 0, runtimedomain.Digest{}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := governedContext()
			mutate(&c)
			if _, _, err := c.Marshal("execution-1"); !errors.Is(err, ErrInvalidContext) {
				t.Fatalf("err = %v, want ErrInvalidContext", err)
			}
		})
	}
	eight := validContext()
	eight.ActorChain = nil
	for range MaxActorChain {
		eight.ActorChain = append(eight.ActorChain, ActorLink{Kind: "agent", Ref: "x"})
	}
	if err := eight.Validate(); err != nil {
		t.Fatalf("eight links are allowed: %v", err)
	}
}

func TestVerifyRefusesTamperedAndUnknownBytes(t *testing.T) {
	encoded, ref, err := validContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Verify(tampered, ref); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered: %v", err)
	}
	unknown := protowire.AppendTag(append([]byte(nil), encoded...), 21, protowire.BytesType)
	unknown = protowire.AppendString(unknown, "x")
	if _, err := Verify(unknown, Reference{Digest: runtimedomain.SHA256(unknown)}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestReferenceFromProtoRefusesForeignContentIDs(t *testing.T) {
	_, ref, err := validContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	message := ref.ReferenceProto()
	message.ContentEntryId = "agent.execution_request"
	if _, err := ReferenceFromProto(message); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("foreign content id: %v", err)
	}
	message = ref.ReferenceProto()
	message.SchemaRevision = "elitea.runtime.security-context.v2"
	if _, err := ReferenceFromProto(message); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("other revision: %v", err)
	}
}

// No field of the context messages may be named like a credential or
// personal data: the context carries references only (issue 1115).
func TestContextFieldsNeverNameASecret(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)token|secret|password|email`)
	messages := []protoreflect.MessageDescriptor{
		(&runtimev1.ExecutionSecurityContextV1{}).ProtoReflect().Descriptor(),
		(&runtimev1.CallerContextV1{}).ProtoReflect().Descriptor(),
		(&runtimev1.ActorLinkV1{}).ProtoReflect().Descriptor(),
		(&runtimev1.ExecutionSecurityContextReferenceV1{}).ProtoReflect().Descriptor(),
		(&runtimev1.GovernedCredentialReferenceV1{}).ProtoReflect().Descriptor(),
	}
	for _, message := range messages {
		fields := message.Fields()
		for i := 0; i < fields.Len(); i++ {
			if name := string(fields.Get(i).Name()); forbidden.MatchString(name) {
				t.Errorf("%s.%s names a credential or personal data", message.Name(), name)
			}
		}
	}
}

// The conformance corpus the Rust worker checks too. Regenerate with
// ELITEA_UPDATE_SECURITY_CONTEXT_VECTORS=1.
type vector struct {
	Name          string `json:"name"`
	ContentID     string `json:"content_id"`
	ContextHex    string `json:"context_hex"`
	DigestHex     string `json:"reference_digest_hex"`
	Expect        string `json:"expect"`
	BindingEpoch  int64  `json:"agent_binding_epoch,omitempty"`
	PolicyVersion string `json:"policy_revision_id,omitempty"`
}

type corpus struct {
	SchemaRevision string   `json:"schema_revision"`
	Vectors        []vector `json:"vectors"`
}

const corpusPath = "../../../../../../testdata/proto/runtime/v1/security-context/vectors.json"

func buildCorpus(t *testing.T) corpus {
	t.Helper()
	encoded, ref, err := validContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 0x01
	governed, governedRef, err := governedContext().Marshal("execution-1")
	if err != nil {
		t.Fatal(err)
	}
	unknown := protowire.AppendTag(append([]byte(nil), encoded...), 18, protowire.BytesType)
	unknown = protowire.AppendString(unknown, "x")
	unknownDigest := runtimedomain.SHA256(unknown)
	return corpus{SchemaRevision: SchemaRevisionV1, Vectors: []vector{
		{Name: "valid governed context", ContentID: ref.ContentID, ContextHex: hex.EncodeToString(encoded), DigestHex: hex.EncodeToString(ref.Digest[:]), Expect: "accept", BindingEpoch: 3, PolicyVersion: "2026-10-r1"},
		{Name: "bytes do not match the reference digest", ContentID: ref.ContentID, ContextHex: hex.EncodeToString(tampered), DigestHex: hex.EncodeToString(ref.Digest[:]), Expect: "digest_mismatch"},
		{Name: "valid governed context with frozen credential references", ContentID: governedRef.ContentID, ContextHex: hex.EncodeToString(governed), DigestHex: hex.EncodeToString(governedRef.Digest[:]), Expect: "accept", BindingEpoch: 3, PolicyVersion: "2026-10-r1"},
		{Name: "unknown field 18", ContentID: ref.ContentID, ContextHex: hex.EncodeToString(unknown), DigestHex: hex.EncodeToString(unknownDigest[:]), Expect: "invalid"},
	}}
}

func TestConformanceCorpus(t *testing.T) {
	want := buildCorpus(t)
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("ELITEA_UPDATE_SECURITY_CONTEXT_VECTORS") == "1" {
		if err := os.MkdirAll(filepath.Dir(corpusPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(corpusPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read corpus (regenerate with ELITEA_UPDATE_SECURITY_CONTEXT_VECTORS=1): %v", err)
	}
	if string(stored) != string(encoded) {
		t.Fatal("the stored corpus differs from the Go encoder; regenerate with ELITEA_UPDATE_SECURITY_CONTEXT_VECTORS=1")
	}
	for _, v := range want.Vectors {
		raw, _ := hex.DecodeString(v.ContextHex)
		digest, _ := hex.DecodeString(v.DigestHex)
		var ref Reference
		copy(ref.Digest[:], digest)
		_, err := Verify(raw, ref)
		switch v.Expect {
		case "accept":
			if err != nil {
				t.Errorf("%s: %v", v.Name, err)
			}
		case "digest_mismatch":
			if !errors.Is(err, ErrDigestMismatch) {
				t.Errorf("%s: err = %v", v.Name, err)
			}
		default:
			if !errors.Is(err, ErrInvalidContext) {
				t.Errorf("%s: err = %v", v.Name, err)
			}
		}
	}
}
