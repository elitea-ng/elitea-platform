package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

type definitionMaterializerFunc func(context.Context, int32, int32, json.RawMessage) (json.RawMessage, error)

func (function definitionMaterializerFunc) MaterializeCurrentApplicationVersion(
	ctx context.Context, projectID int32, actorID int32, frozen json.RawMessage,
) (json.RawMessage, error) {
	return function(ctx, projectID, actorID, frozen)
}

func TestRuntimeApplicationDefinitionDigestV1Fixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "runtime_application_definition_digest_v1.json"))
	require.NoError(t, err)
	var fixture struct {
		SchemaVersion string `json:"schema_version"`
		Domain        string `json:"domain_utf8"`
		ProjectID     int64  `json:"project_id"`
		ApplicationID uint64 `json:"application_id"`
		VersionID     uint64 `json:"version_id"`
		Frozen        string `json:"frozen_pre_redemption_json"`
		FrozenLength  int    `json:"frozen_pre_redemption_length"`
		Digest        string `json:"frozen_definition_sha256"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Equal(t, "elitea.runtime.application-definition-digest.fixture.v1", fixture.SchemaVersion)
	require.Equal(t, runtimeApplicationDefinitionDigestDomain, fixture.Domain)
	require.Equal(t, len([]byte(fixture.Frozen)), fixture.FrozenLength)
	digest, err := runtimeApplicationDefinitionSHA256(fixture.ProjectID, fixture.ApplicationID, fixture.VersionID, json.RawMessage(fixture.Frozen))
	require.NoError(t, err)
	require.Equal(t, fixture.Digest, digest)
	require.Equal(t, "31e7aff6b702375b6eaca24b124cf465ff9b9c587756b655e53b8256342395d5", digest)
}

func TestRuntimeApplicationDefinitionDigestBindsExactBytesAndIdentities(t *testing.T) {
	t.Parallel()
	frozen := json.RawMessage(`{"agent_type":"agent","tools":[]}`)
	base, err := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
	require.NoError(t, err)
	for _, test := range []struct {
		name                 string
		project              int64
		application, version uint64
		frozen               json.RawMessage
	}{
		{"project", 18, 31, 41, frozen},
		{"application", 17, 32, 41, frozen},
		{"version", 17, 31, 42, frozen},
		{"whitespace", 17, 31, 41, json.RawMessage(`{ "agent_type":"agent","tools":[]}`)},
		{"key_order", 17, 31, 41, json.RawMessage(`{"tools":[],"agent_type":"agent"}`)},
		{"definition", 17, 31, 41, json.RawMessage(`{"agent_type":"pipeline","tools":[]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest, err := runtimeApplicationDefinitionSHA256(test.project, test.application, test.version, test.frozen)
			require.NoError(t, err)
			require.NotEqual(t, base, digest)
		})
	}
}

func TestRuntimeApplicationDefinitionDigestRejectsInvalidFrozenInput(t *testing.T) {
	t.Parallel()
	for _, frozen := range []json.RawMessage{nil, json.RawMessage(`invalid`), json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`true`), json.RawMessage(`{}`), json.RawMessage(`{"value":"` + strings.Repeat("a", maxRuntimeApplicationVersionResponseBytes) + `"}`)} {
		digest, err := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
		require.Error(t, err)
		require.Empty(t, digest)
		require.EqualError(t, err, "frozen application definition is invalid")
	}
	for _, identity := range []struct {
		project              int64
		application, version uint64
	}{
		{0, 31, 41}, {-1, 31, 41}, {math.MaxInt32 + 1, 31, 41},
		{17, 0, 41}, {17, math.MaxInt32 + 1, 41}, {17, 31, 0}, {17, 31, math.MaxInt32 + 1},
	} {
		digest, err := runtimeApplicationDefinitionSHA256(identity.project, identity.application, identity.version, json.RawMessage(`{"tools":[]}`))
		require.Error(t, err)
		require.Empty(t, digest)
	}
}

func TestRuntimeApplicationDefinitionProducerHashesBeforeCredentialRedemption(t *testing.T) {
	t.Parallel()
	frozen := json.RawMessage(`{"agent_type":"agent","instructions":"Return fixture","tools":[]}`)
	expected, err := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
	require.NoError(t, err)
	var previousDetails json.RawMessage
	for _, secret := range []string{"synthetic-credential-one", "synthetic-credential-two"} {
		materializer := definitionMaterializerFunc(func(_ context.Context, project, actor int32, received json.RawMessage) (json.RawMessage, error) {
			require.EqualValues(t, 17, project)
			require.EqualValues(t, 11, actor)
			require.Equal(t, frozen, received)
			// Mutation proves that the producer takes identity before redemption.
			for index := range received {
				received[index] = 'x'
			}
			return json.Marshal(map[string]any{"agent_type": "agent", "credential": secret})
		})
		service := definitionServiceForTest(t, frozen, materializer)
		result, err := service.Resolve(t.Context(), ContentClaim{}, 31, 41)
		require.NoError(t, err)
		require.Equal(t, expected, result.FrozenDefinitionSHA256)
		require.Contains(t, string(result.VersionDetails), secret)
		if previousDetails != nil {
			require.NotEqual(t, previousDetails, result.VersionDetails)
		}
		previousDetails = result.VersionDetails
	}
}

func TestRuntimeApplicationDefinitionProducerNeverSelectsEditableDigestMetadata(t *testing.T) {
	t.Parallel()
	forged := strings.Repeat("f", 64)
	frozen, err := json.Marshal(map[string]any{"agent_type": "agent", "name": "editable-alias", "meta": map[string]any{"frozen_definition_sha256": forged, "definition_digest": forged}, "tools": []any{}})
	require.NoError(t, err)
	expected, err := runtimeApplicationDefinitionSHA256(17, 31, 41, frozen)
	require.NoError(t, err)
	service := definitionServiceForTest(t, frozen, definitionMaterializerFunc(func(_ context.Context, _, _ int32, received json.RawMessage) (json.RawMessage, error) {
		return received, nil
	}))
	result, err := service.Resolve(t.Context(), ContentClaim{}, 31, 41)
	require.NoError(t, err)
	require.Equal(t, expected, result.FrozenDefinitionSHA256)
	require.NotEqual(t, forged, result.FrozenDefinitionSHA256)
}

func TestRuntimeApplicationDefinitionProducerRejectsBeforeRedemption(t *testing.T) {
	t.Parallel()
	for _, frozen := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{"broken":`), json.RawMessage(`[]`), json.RawMessage(`{"value":"` + strings.Repeat("a", maxRuntimeApplicationVersionResponseBytes) + `"}`)} {
		calls := 0
		service := definitionServiceForTest(t, frozen, definitionMaterializerFunc(func(context.Context, int32, int32, json.RawMessage) (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{"ok":true}`), nil
		}))
		result, err := service.Resolve(t.Context(), ContentClaim{}, 31, 41)
		require.ErrorIs(t, err, ErrContentUnavailable)
		require.Equal(t, runtimeContextStageNestedVersionFreeze, runtimeContextUnavailableStage(err))
		require.Zero(t, calls)
		require.Empty(t, result.FrozenDefinitionSHA256)
	}
}

func TestRuntimeApplicationDefinitionProducerRetainsMaterializerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service := definitionServiceForTest(t, json.RawMessage(`{"tools":[]}`), definitionMaterializerFunc(func(context.Context, int32, int32, json.RawMessage) (json.RawMessage, error) {
		cancel()
		return nil, context.Canceled
	}))
	result, err := service.Resolve(ctx, ContentClaim{}, 31, 41)
	require.True(t, errors.Is(err, context.Canceled))
	require.Empty(t, result.FrozenDefinitionSHA256)
}

func TestRuntimeApplicationDefinitionLegacyEnvelopeOmitsIdentity(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(RuntimeApplicationVersionContext{SchemaVersion: RuntimeApplicationVersionSchemaVersion, ProjectID: 17, ApplicationID: 31, VersionID: 41, VersionDetails: json.RawMessage(`{"tools":[]}`)})
	require.NoError(t, err)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &envelope))
	require.Len(t, envelope, 5)
	require.NotContains(t, envelope, "frozen_definition_sha256")
}

func TestRuntimeApplicationDefinitionSchemaRejectsMalformedIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "..", "..", "libs", "jsonschema", "runtime", "v1", "application-version.schema.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var schemaDocument any
	require.NoError(t, json.Unmarshal(raw, &schemaDocument))
	compiler := jsonschema.NewCompiler()
	const schemaID = "https://schemas.elitea.ai/runtime/v1/application-version.schema.json"
	require.NoError(t, compiler.AddResource(schemaID, schemaDocument))
	schema, err := compiler.Compile(schemaID)
	require.NoError(t, err)
	for _, test := range []struct {
		name    string
		present bool
		digest  any
		extra   bool
		valid   bool
	}{
		{"legacy", false, nil, false, true}, {"present", true, strings.Repeat("a", 64), false, true},
		{"zero", true, strings.Repeat("0", 64), false, true}, {"null", true, nil, false, false},
		{"uppercase", true, strings.Repeat("A", 64), false, false}, {"short", true, strings.Repeat("a", 63), false, false},
		{"nonhex", true, strings.Repeat("g", 64), false, false}, {"prefix", true, "sha256:" + strings.Repeat("a", 64), false, false},
		{"number", true, 42, false, false}, {"newline", true, strings.Repeat("a", 64) + "\n", false, false},
		{"extra", false, nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope := map[string]any{"schema_version": RuntimeApplicationVersionSchemaVersion, "project_id": float64(17), "application_id": float64(31), "version_id": float64(41), "version_details": map[string]any{"tools": []any{}}}
			if test.present {
				envelope["frozen_definition_sha256"] = test.digest
			}
			if test.extra {
				envelope["editable_alias"] = "agent-name"
			}
			err := schema.Validate(envelope)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func definitionServiceForTest(t *testing.T, frozen json.RawMessage, materializer CurrentApplicationVersionMaterializer) *RuntimeApplicationVersionService {
	t.Helper()
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 17, ActorID: "11"}, nil
	})
	source := currentApplicationVersionSourceFunc(func(_ context.Context, project int64, application int64, version int64) (CurrentApplicationVersionRecord, error) {
		require.EqualValues(t, 17, project)
		require.EqualValues(t, 31, application)
		require.EqualValues(t, 41, version)
		return CurrentApplicationVersionRecord{ApplicationID: 31, VersionID: 41, VersionDetails: json.RawMessage(`{"stored":true}`)}, nil
	})
	freezer := currentApplicationVersionFreezerFunc(func(_ context.Context, request agentexecutionapp.CurrentApplicationVersionFreezeRequest) (json.RawMessage, error) {
		require.EqualValues(t, 17, request.ProjectID)
		require.EqualValues(t, 11, request.ActorUserID)
		return append(json.RawMessage(nil), frozen...), nil
	})
	service, err := NewRuntimeApplicationVersionService(authorizer, source, freezer, materializer)
	require.NoError(t, err)
	return service
}
