package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/stretchr/testify/require"
)

type currentApplicationVersionSourceFunc func(
	context.Context,
	int64,
	int64,
	int64,
) (CurrentApplicationVersionRecord, error)

func (f currentApplicationVersionSourceFunc) ReadCurrentApplicationVersion(
	ctx context.Context,
	projectID int64,
	applicationID int64,
	versionID int64,
) (CurrentApplicationVersionRecord, error) {
	return f(ctx, projectID, applicationID, versionID)
}

type currentApplicationVersionFreezerFunc func(
	context.Context,
	agentexecutionapp.CurrentApplicationVersionFreezeRequest,
) (json.RawMessage, error)

func (f currentApplicationVersionFreezerFunc) FreezeCurrentApplicationVersion(
	ctx context.Context,
	request agentexecutionapp.CurrentApplicationVersionFreezeRequest,
) (json.RawMessage, error) {
	return f(ctx, request)
}

// The four stubs below stand in for the freezer's collaborators only. The
// freezer itself is the PRODUCTION CurrentApplicationToolSnapshotService in
// every test that asserts on the frozen document, because the point of those
// assertions is what the real freeze does to a child definition — a stub
// freezer would only assert that this file agrees with itself.
type nestedVersionToolkitSettingsStub struct {
	result map[string]any
}

func (stub nestedVersionToolkitSettingsStub) Resolve(
	_ context.Context,
	_ configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	return stub.result, nil
}

type nestedVersionToolkitNameStub struct {
	result string
}

func (stub nestedVersionToolkitNameStub) ResolveCurrentAgentToolkitName(
	_ context.Context,
	_ agentexecutionapp.CurrentAgentToolkitNameRequest,
) (string, error) {
	return stub.result, nil
}

type nestedVersionModelCatalogStub struct {
	queries []configurationapp.CurrentModelCatalogQuery
}

func (stub *nestedVersionModelCatalogStub) Get(
	_ context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	stub.queries = append(stub.queries, query)
	compatible := true
	reasoning := false
	return configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{{
			Name: "model", ProjectID: 90106,
			OpenAICompatible: &compatible, SupportsReasoning: &reasoning,
		}},
	}, nil
}

type nestedVersionGuardrailStub struct{}

func (nestedVersionGuardrailStub) ResolveCurrentAgentGuardrails(
	context.Context,
) (guardrails.Policy, error) {
	return guardrails.Policy{}, nil
}

const nestedVersionStoredDetails = `{
  "id": 2,
  "application_id": 1,
  "name": "RustProbe",
  "status": "all",
  "agent_type": "openai",
  "instructions": "Answer the parent agent.",
  "welcome_message": "",
  "llm_settings": {"model_name": "model"},
  "meta": {"internal_tools": ["internal_mcp", "ask_user"], "step_limit": 25},
  "conversation_starters": [],
  "pipeline_settings": {},
  "author_id": 11,
  "tools": [],
  "skills": [],
  "tags": [],
  "variables": []
}`

func TestNestedApplicationVersionServesTheFrozenClaimScopedDefinition(t *testing.T) {
	t.Parallel()

	fence := bytes.Repeat([]byte{4}, sha256.Size)
	certificate := &x509.Certificate{}
	readCalls := 0
	source := currentApplicationVersionSourceFunc(func(
		_ context.Context,
		projectID int64,
		applicationID int64,
		versionID int64,
	) (CurrentApplicationVersionRecord, error) {
		readCalls++
		// The project comes from the claim, never from the request; the two
		// identity arguments are the only thing the caller selects.
		require.EqualValues(t, 90106, projectID)
		require.EqualValues(t, 1, applicationID)
		require.EqualValues(t, 2, versionID)
		return CurrentApplicationVersionRecord{
			ApplicationID:  1,
			VersionID:      2,
			VersionDetails: json.RawMessage(nestedVersionStoredDetails),
		}, nil
	})
	server := newNestedApplicationVersionTestServer(t, source, newNestedVersionFreezer(t))

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nestedApplicationVersionRequest(t, certificate, fence, "1", "2"))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, 1, readCalls)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store, no-cache, must-revalidate", response.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", response.Header().Get("Pragma"))
	require.Equal(t, "0", response.Header().Get("Expires"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.NotEmpty(t, response.Header().Get("Content-Length"))
	digest := sha256.Sum256(response.Body.Bytes())
	require.Equal(t, formatSHA256Digest(digest), response.Header().Get("Content-Digest"))

	// The client decodes with `deny_unknown_fields`
	// (services/elitea-worker-rust/src/transport/runtime_context.rs:773-781), so
	// the key SET is part of the contract, not only the values.
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Len(t, envelope, 5)
	for _, key := range []string{
		"schema_version", "project_id", "application_id", "version_id", "version_details",
	} {
		require.Contains(t, envelope, key)
	}

	var value RuntimeApplicationVersionContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &value))
	require.Equal(t, RuntimeApplicationVersionSchemaVersion, value.SchemaVersion)
	require.EqualValues(t, 90106, value.ProjectID)
	require.EqualValues(t, 1, value.ApplicationID)
	require.EqualValues(t, 2, value.VersionID)

	var frozen map[string]any
	require.NoError(t, json.Unmarshal(value.VersionDetails, &frozen))
	// The freeze ran: `openai` is normalized to the runtime's `agent`, which is
	// the only value ApplicationAssemblyState::build admits for a nested
	// reference (services/elitea-worker-rust/src/agents/application_tools.rs).
	require.Equal(t, "agent", frozen["agent_type"])
	internalTools := 0
	for _, raw := range frozen["tools"].([]any) {
		tool := raw.(map[string]any)
		if strings.HasPrefix(tool["type"].(string), "mcp_elitea_internal_") {
			internalTools++
			require.Contains(t, tool["settings"].(map[string]any), "url")
		}
	}
	require.Positive(t, internalTools)
	meta, ok := frozen["meta"].(map[string]any)
	require.True(t, ok)
	// `internal_mcp` is dropped because the native runtime's catalogue would
	// refuse the whole profile over it...
	require.Equal(t, []any{"ask_user"}, meta["internal_tools"])
	// ...while `step_limit` is PRESERVED. It is not stripped by anything in the
	// freeze, and the worker validates it in place
	// (validate_application_meta_step_limit, src/agents/assembly.rs).
	require.EqualValues(t, 25, meta["step_limit"])
	// Model resolution is part of the freeze, so the child's settings come back
	// resolved rather than as the author typed them.
	llmSettings, ok := frozen["llm_settings"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "model", llmSettings["model_name"])
	require.EqualValues(t, 90106, llmSettings["model_project_id"])
	require.Equal(t, true, llmSettings["openai_compatible"])
}

func TestNestedApplicationVersionRefusesAClaimItCannotAuthorize(t *testing.T) {
	t.Parallel()

	source := currentApplicationVersionSourceFunc(func(
		context.Context, int64, int64, int64,
	) (CurrentApplicationVersionRecord, error) {
		t.Fatal("an unauthorized claim must not reach the tenant schema")
		return CurrentApplicationVersionRecord{}, nil
	})
	freezer := currentApplicationVersionFreezerFunc(func(
		context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
	) (json.RawMessage, error) {
		t.Fatal("an unauthorized claim must not be frozen")
		return nil, nil
	})

	// No verified peer certificate and no claim headers: the request never
	// identifies an execution, which is indistinguishable from a forged one.
	anonymous, err := NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("the nested route must not call content-entry authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("the nested route must not open input content")
			return nil, nil
		}),
		contentMaterializerFunc(func(
			context.Context, ContentAuthorization, []byte, int64,
		) ([]byte, error) {
			t.Fatal("the nested route must not materialize input content")
			return nil, nil
		}),
		&EliteaClientTokenService{},
		nestedApplicationVersionService(
			t,
			agentRuntimeContextAuthorizerFunc(func(
				context.Context, ContentClaim,
			) (RuntimeContextAuthorization, error) {
				t.Fatal("a request without a claim must not be authorized")
				return RuntimeContextAuthorization{}, nil
			}),
			source,
			freezer,
		),
		1024,
		1,
	)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	anonymous.Routes().ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/executions/execution-1/generations/1/runtime-context/applications/1/versions/2",
		nil,
	))
	require.Equal(t, http.StatusUnauthorized, response.Code)

	// A well-formed claim that does not belong to this execution/generation:
	// the authorizer's row simply does not match, and the answer must not say
	// which half was wrong.
	rejected := newNestedApplicationVersionTestServerWithAuthorizer(
		t,
		agentRuntimeContextAuthorizerFunc(func(
			_ context.Context, claim ContentClaim,
		) (RuntimeContextAuthorization, error) {
			require.Equal(t, "execution-1", claim.ExecutionID)
			require.EqualValues(t, 1, claim.Generation)
			return RuntimeContextAuthorization{}, ErrContentUnauthorized
		}),
		source,
		freezer,
	)
	response = httptest.NewRecorder()
	rejected.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.NotContains(t, response.Body.String(), "execution")

	// An authorizer that failed for its own reasons is a dependency failure,
	// which the worker may retry — not a refusal, which it may not.
	unavailable := newNestedApplicationVersionTestServerWithAuthorizer(
		t,
		agentRuntimeContextAuthorizerFunc(func(
			context.Context, ContentClaim,
		) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{}, errNestedVersionTest
		}),
		source,
		freezer,
	)
	response = httptest.NewRecorder()
	unavailable.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestNestedApplicationVersionRefusesAnIdentityItCannotServe(t *testing.T) {
	t.Parallel()

	freezer := currentApplicationVersionFreezerFunc(func(
		context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
	) (json.RawMessage, error) {
		t.Fatal("a version that does not match the request must not be frozen")
		return nil, nil
	})

	// The stored row belongs to a DIFFERENT application than the URL named. The
	// query filters on both columns, so this can only happen if that filter is
	// ever relaxed — which is exactly the regression this asserts against.
	mismatched := newNestedApplicationVersionTestServer(
		t,
		currentApplicationVersionSourceFunc(func(
			context.Context, int64, int64, int64,
		) (CurrentApplicationVersionRecord, error) {
			return CurrentApplicationVersionRecord{
				ApplicationID:  9,
				VersionID:      2,
				VersionDetails: json.RawMessage(nestedVersionStoredDetails),
			}, nil
		}),
		freezer,
	)
	response := httptest.NewRecorder()
	mismatched.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusNotFound, response.Code)

	// No such application/version pair in this project's schema.
	absent := newNestedApplicationVersionTestServer(
		t,
		currentApplicationVersionSourceFunc(func(
			context.Context, int64, int64, int64,
		) (CurrentApplicationVersionRecord, error) {
			return CurrentApplicationVersionRecord{}, ErrContentNotFound
		}),
		freezer,
	)
	response = httptest.NewRecorder()
	absent.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusNotFound, response.Code)

	// Non-canonical and out-of-range path identities never reach the schema.
	unreachable := newNestedApplicationVersionTestServer(
		t,
		currentApplicationVersionSourceFunc(func(
			context.Context, int64, int64, int64,
		) (CurrentApplicationVersionRecord, error) {
			t.Fatal("a malformed identity must not reach the tenant schema")
			return CurrentApplicationVersionRecord{}, nil
		}),
		freezer,
	)
	for _, identity := range [][2]string{
		{"01", "2"},
		{"1", "0"},
		{"+1", "2"},
		{"1", "two"},
		{"1", "18446744073709551616"},
	} {
		response = httptest.NewRecorder()
		unreachable.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), identity[0], identity[1],
		))
		require.Equal(t, http.StatusBadRequest, response.Code, identity)
	}

	// An identity that is canonical but larger than the tenant schema's own
	// integer columns is a miss, not a malformed request.
	response = httptest.NewRecorder()
	unreachable.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2147483648",
	))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestNestedApplicationVersionRefusesRatherThanTruncateAnOversizeDefinition(t *testing.T) {
	t.Parallel()

	oversize := make([]byte, maxRuntimeApplicationVersionResponseBytes)
	for index := range oversize {
		oversize[index] = 'a'
	}
	server := newNestedApplicationVersionTestServer(
		t,
		currentApplicationVersionSourceFunc(func(
			context.Context, int64, int64, int64,
		) (CurrentApplicationVersionRecord, error) {
			return CurrentApplicationVersionRecord{
				ApplicationID:  1,
				VersionID:      2,
				VersionDetails: json.RawMessage(nestedVersionStoredDetails),
			}, nil
		}),
		currentApplicationVersionFreezerFunc(func(
			context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
		) (json.RawMessage, error) {
			encoded, err := json.Marshal(map[string]any{"instructions": string(oversize)})
			require.NoError(t, err)
			return encoded, nil
		}),
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NotContains(t, response.Body.String(), "instructions")
	require.Less(t, response.Body.Len(), maxRuntimeApplicationVersionResponseBytes)
}

func TestNestedApplicationVersionRouteIsAbsentWithoutItsService(t *testing.T) {
	t.Parallel()

	server, err := NewRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("an unregistered route must not authorize content")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("an unregistered route must not open content")
			return nil, nil
		}),
		&EliteaClientTokenService{},
		1024,
		1,
	)
	require.NoError(t, err)
	require.Nil(t, server.runtimeVersions)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "1", "2",
	))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestNestedApplicationVersionCompositionRequiresEveryDependency(t *testing.T) {
	t.Parallel()

	authorizer := agentRuntimeContextAuthorizerFunc(func(
		context.Context, ContentClaim,
	) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{}, nil
	})
	source := currentApplicationVersionSourceFunc(func(
		context.Context, int64, int64, int64,
	) (CurrentApplicationVersionRecord, error) {
		return CurrentApplicationVersionRecord{}, nil
	})
	freezer := currentApplicationVersionFreezerFunc(func(
		context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
	) (json.RawMessage, error) {
		return nil, nil
	})

	_, err := NewRuntimeApplicationVersionService(nil, source, freezer, nestedVersionMaterializerForTest(t))
	require.EqualError(t, err, "runtime application version dependencies are required")
	_, err = NewRuntimeApplicationVersionService(authorizer, nil, freezer, nestedVersionMaterializerForTest(t))
	require.EqualError(t, err, "runtime application version dependencies are required")
	_, err = NewRuntimeApplicationVersionService(authorizer, source, nil, nestedVersionMaterializerForTest(t))
	require.EqualError(t, err, "runtime application version dependencies are required")
	_, err = NewRuntimeApplicationVersionService(authorizer, source, freezer, nil)
	require.EqualError(t, err, "runtime application version dependencies are required")
	service, err := NewRuntimeApplicationVersionService(authorizer, source, freezer, nestedVersionMaterializerForTest(t))
	require.NoError(t, err)

	contentAuthorizer := contentAuthorizerFunc(func(
		context.Context, ContentClaim,
	) (ContentAuthorization, error) {
		return ContentAuthorization{}, nil
	})
	store := contentStoreFunc(func(
		context.Context, string, string, string, string,
	) (io.ReadCloser, error) {
		return nil, nil
	})
	materializer := contentMaterializerFunc(func(
		context.Context, ContentAuthorization, []byte, int64,
	) ([]byte, error) {
		return nil, nil
	})
	runtimeToken := &EliteaClientTokenService{}

	_, err = NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizer, store, nil, runtimeToken, service, 1024, 1,
	)
	require.EqualError(t, err, "content materializer is required")
	_, err = NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizer, store, materializer, nil, service, 1024, 1,
	)
	require.EqualError(t, err, "runtime context is required")
	_, err = NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizer, store, materializer, runtimeToken, nil, 1024, 1,
	)
	require.EqualError(t, err, "runtime application version context is required")
	server, err := NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizer, store, materializer, runtimeToken, service, 1024, 1,
	)
	require.NoError(t, err)
	require.Same(t, service, server.runtimeVersions)
	require.Same(t, runtimeToken, server.runtimeToken)
}

var errNestedVersionTest = &nestedVersionTestError{}

type nestedVersionTestError struct{}

func (*nestedVersionTestError) Error() string { return "nested version dependency failed" }

// newNestedVersionFreezer builds the production freeze with stub collaborators
// so a test can assert on what the freeze actually does today.
func newNestedVersionFreezer(t *testing.T) agentexecutionapp.CurrentApplicationVersionFreezer {
	t.Helper()
	freezer, err := agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		nestedVersionToolkitSettingsStub{result: map[string]any{}},
		nestedVersionToolkitNameStub{result: "toolkit"},
		&nestedVersionModelCatalogStub{},
		nestedVersionGuardrailStub{},
		1,
	)
	require.NoError(t, err)
	return freezer
}

func newNestedApplicationVersionTestServer(
	t *testing.T,
	source CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
) *ContentServer {
	t.Helper()
	return newNestedApplicationVersionTestServerWithAuthorizer(
		t,
		agentRuntimeContextAuthorizerFunc(func(
			context.Context, ContentClaim,
		) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{
				ResourceProjectID: 90106,
				ActorID:           "11",
				Initiator:         runtimeContextInitiatorUser,
			}, nil
		}),
		source,
		freezer,
	)
}

func newNestedApplicationVersionTestServerWithAuthorizer(
	t *testing.T,
	authorizer AgentRuntimeContextAuthorizer,
	source CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
) *ContentServer {
	t.Helper()
	server, err := NewNestedAgentRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("the nested route must not call content-entry authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("the nested route must not open input content")
			return nil, nil
		}),
		contentMaterializerFunc(func(
			context.Context, ContentAuthorization, []byte, int64,
		) ([]byte, error) {
			t.Fatal("the nested route must not materialize input content")
			return nil, nil
		}),
		&EliteaClientTokenService{},
		nestedApplicationVersionService(t, authorizer, source, freezer),
		1024,
		1,
	)
	require.NoError(t, err)
	return server
}

func nestedApplicationVersionService(
	t *testing.T,
	authorizer AgentRuntimeContextAuthorizer,
	source CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
) *RuntimeApplicationVersionService {
	t.Helper()
	service, err := NewRuntimeApplicationVersionService(authorizer, source, freezer, nestedVersionMaterializerForTest(t))
	require.NoError(t, err)
	return service
}

func nestedApplicationVersionRequest(
	t *testing.T,
	certificate *x509.Certificate,
	fence []byte,
	applicationID string,
	versionID string,
) *http.Request {
	t.Helper()
	path := strings.Join([]string{
		"/executions/execution-1/generations/1/runtime-context/applications",
		applicationID,
		"versions",
		versionID,
	}, "/")
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}
	request.Header.Set(claimIDHeader, "claim-1")
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(fence))
	return request
}

func nestedVersionMaterializerForTest(t *testing.T) *CurrentConfigurationsMaterializer {
	t.Helper()
	materializer, err := NewCurrentAgentConfigurationsMaterializer(&currentMaterializationUnsecreterStub{}, &currentAgentPrebuiltMCPResolverStub{found: true, result: map[string]any{"url": "https://main.example.test/app/90106/mcp/elitea_core/skills", "headers": map[string]any{"Authorization": "Bearer runtime-only-token"}}})
	require.NoError(t, err)
	return materializer
}
