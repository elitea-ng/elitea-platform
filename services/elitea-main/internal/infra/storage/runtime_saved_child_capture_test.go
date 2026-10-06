package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

type savedChildCaptureFixture struct {
	t              *testing.T
	source, frozen []byte
	full           string
	called         bool
	fail           bool
}

func (c *savedChildCaptureFixture) CaptureFrozenSavedChildVersion(_ context.Context, _ ContentClaim, app, version uint64, full string, frozen, source json.RawMessage) error {
	c.t.Helper()
	if app != 31 || version != 41 || !bytes.Equal(source, c.source) || !bytes.Equal(frozen, c.frozen) || full != c.full {
		c.t.Fatal("capture mixed original source with frozen runtime identity")
	}
	c.called = true
	if c.fail {
		return scope.ErrDenied
	}
	return nil
}
func TestSavedChildProducerRetainsOriginalSourceBeforeFreezeAndCredentials(t *testing.T) {
	source := json.RawMessage(`{"agent_type":"agent","instructions":"Original saved declaration","tools":[]}`)
	frozen := json.RawMessage(`{"agent_type":"agent","instructions":"Original saved declaration","tools":[],"runtime_static_settings":true}`)
	full, _ := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "capture refuses"}[fail], func(t *testing.T) {
			capture := &savedChildCaptureFixture{t: t, source: source, frozen: frozen, full: full, fail: fail}
			redeemed := false
			service, err := NewRuntimeApplicationVersionService(agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
				return RuntimeContextAuthorization{ResourceProjectID: 17, ActorID: "11"}, nil
			}), currentApplicationVersionSourceFunc(func(context.Context, int64, int64, int64) (CurrentApplicationVersionRecord, error) {
				return CurrentApplicationVersionRecord{ApplicationID: 31, VersionID: 41, VersionDetails: bytes.Clone(source)}, nil
			}), currentApplicationVersionFreezerFunc(func(_ context.Context, request agentexecutionapp.CurrentApplicationVersionFreezeRequest) (json.RawMessage, error) {
				if !bytes.Equal(request.VersionDetails, source) {
					t.Fatal("source replaced before freeze")
				}
				return bytes.Clone(frozen), nil
			}), definitionMaterializerFunc(func(_ context.Context, _, _ int32, received json.RawMessage) (json.RawMessage, error) {
				if !capture.called {
					t.Fatal("credential redemption preceded source capture")
				}
				redeemed = true
				return received, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			service.WithFrozenSavedChildVersionCapture(capture)
			result, err := service.Resolve(t.Context(), ContentClaim{}, 31, 41)
			if fail {
				if err == nil || redeemed {
					t.Fatal("denied source redeemed credentials")
				}
				return
			}
			if err != nil || !redeemed || result.FrozenDefinitionSHA256 != full {
				t.Fatal(result, err)
			}
			sourceWire, err := scope.NewSourceWire(17, 11, 31, 41, source)
			if err != nil || sourceWire.SourceDefinitionSHA256 == result.FrozenDefinitionSHA256 {
				t.Fatal("runtime fingerprint substituted for source fingerprint")
			}
		})
	}
}
func TestSavedChildProducerDisabledKeepsExistingFrozenBytes(t *testing.T) {
	frozen := json.RawMessage(`{"agent_type":"agent","instructions":"Existing runtime version","tools":[]}`)
	service := definitionServiceForTest(t, frozen, definitionMaterializerFunc(func(_ context.Context, _, _ int32, received json.RawMessage) (json.RawMessage, error) {
		return received, nil
	}))
	result, err := service.Resolve(t.Context(), ContentClaim{}, 31, 41)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
	if !bytes.Equal(result.VersionDetails, frozen) || result.FrozenDefinitionSHA256 != expected {
		t.Fatal("disabled capture changed old saved-version bytes")
	}
}
