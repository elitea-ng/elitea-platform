package storage

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The two chat-authored builder WRITE routes (#940 A8). Everything here is
// about the one property that makes a write on this listener safe: the project
// comes from the CLAIM and from nowhere else, and the request body carries only
// content.

type builderSkillSinkFunc func(
	context.Context, int64, string, string, string,
) (RuntimeSkillRecord, error)

func (f builderSkillSinkFunc) UpsertRuntimeSkillByName(
	ctx context.Context,
	projectID int64,
	name string,
	description string,
	instructions string,
) (RuntimeSkillRecord, error) {
	return f(ctx, projectID, name, description, instructions)
}

// builderProjectContextSink records what it was asked to write so a test can
// assert the KEPT `enabled` value, which is the one thing an update has to get
// right that a create does not.
type builderProjectContextSink struct {
	existing       RuntimeProjectContextRecord
	readErr        error
	writeErr       error
	sawProjectID   int64
	sawContent     string
	sawEnabled     bool
	writeCallCount int
}

func (s *builderProjectContextSink) ReadRuntimeProjectContext(
	context.Context, int64,
) (RuntimeProjectContextRecord, error) {
	if s.readErr != nil {
		return RuntimeProjectContextRecord{}, s.readErr
	}
	return s.existing, nil
}

func (s *builderProjectContextSink) WriteRuntimeProjectContext(
	_ context.Context, projectID int64, content string, enabled bool,
) error {
	s.writeCallCount++
	s.sawProjectID = projectID
	s.sawContent = content
	s.sawEnabled = enabled
	return s.writeErr
}

// TestSkillWriteRouteTakesTheProjectFromTheClaimNotTheRequest is the happy path
// and the tenancy proof at once: the sink is handed 4242, the authorizer's
// resource_project_id, while the request body names no project at all.
func TestSkillWriteRouteTakesTheProjectFromTheClaimNotTheRequest(t *testing.T) {
	t.Parallel()

	var sawProject int64
	var sawName, sawDescription, sawInstructions string
	server := newBuilderTestServer(
		t,
		builderAuthorizerForProject(4242),
		builderSkillSinkFunc(func(
			_ context.Context, projectID int64, name, description, instructions string,
		) (RuntimeSkillRecord, error) {
			sawProject = projectID
			sawName, sawDescription, sawInstructions = name, description, instructions
			return RuntimeSkillRecord{SkillID: "77", Name: name, Created: true}, nil
		}),
		&builderProjectContextSink{},
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "skills", `{
		"name": "Daily Standup Summarizer",
		"description": "Summarizes daily standup notes into action items",
		"instructions": "Extract blockers, decisions and action items."
	}`))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, int64(4242), sawProject)
	require.Equal(t, "Daily Standup Summarizer", sawName)
	require.Equal(t, "Summarizes daily standup notes into action items", sawDescription)
	require.Equal(t, "Extract blockers, decisions and action items.", sawInstructions)

	var body RuntimeSkillWriteContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	// The schema discriminator is the first thing the worker compares; a
	// response that is otherwise perfect and carries the wrong one is refused
	// there with a message that names nothing.
	require.Equal(t, RuntimeSkillWriteSchemaVersion, body.SchemaVersion)
	require.Equal(t, int64(4242), body.ProjectID)
	require.Equal(t, "77", body.SkillID)
	require.True(t, body.Created)
}

// A body that names a project must not change where the write lands. It is
// refused outright (DisallowUnknownFields), which is stronger than ignoring it:
// a silently-dropped field is how a caller comes to believe it works.
func TestSkillWriteRouteRefusesARequestThatNamesAProject(t *testing.T) {
	t.Parallel()

	server := newBuilderTestServer(
		t,
		builderAuthorizerForProject(4242),
		builderSkillSinkFunc(func(
			context.Context, int64, string, string, string,
		) (RuntimeSkillRecord, error) {
			t.Fatal("a request carrying an unknown field must not reach the sink")
			return RuntimeSkillRecord{}, nil
		}),
		&builderProjectContextSink{},
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "skills", `{
		"name": "Anything", "instructions": "Do something", "project_id": 9
	}`))
	require.Equal(t, http.StatusBadRequest, response.Code)
}

// An empty name or empty instructions is 422, not 500 and not a silent
// success: the document is exactly what it claims to be and this route will
// never store it, which is the distinction the worker turns into "write less"
// rather than "the platform is down".
func TestSkillWriteRouteRefusesAnUnstorableDocumentWith422(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"no name":         `{"name": "  ", "instructions": "Do something"}`,
		"no instructions": `{"name": "Named", "instructions": "   "}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newBuilderTestServer(
				t,
				builderAuthorizerForProject(4242),
				builderSkillSinkFunc(func(
					context.Context, int64, string, string, string,
				) (RuntimeSkillRecord, error) {
					t.Fatal("an unstorable document must not reach the sink")
					return RuntimeSkillRecord{}, nil
				}),
				&builderProjectContextSink{},
			)
			response := httptest.NewRecorder()
			server.Routes().ServeHTTP(response, builderRequest(t, "skills", body))
			require.Equal(t, http.StatusUnprocessableEntity, response.Code)
		})
	}
}

// A refused claim is 403 and never reaches the sink. Asserted on a WRITE
// because the cost of getting it wrong here is a row in somebody else's
// project, not a leaked read.
func TestBuilderRoutesRefuseAnUnauthorizedClaimBeforeWriting(t *testing.T) {
	t.Parallel()

	contextSink := &builderProjectContextSink{}
	server := newBuilderTestServer(
		t,
		agentRuntimeContextAuthorizerFunc(func(
			context.Context, ContentClaim,
		) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{}, ErrContentUnauthorized
		}),
		builderSkillSinkFunc(func(
			context.Context, int64, string, string, string,
		) (RuntimeSkillRecord, error) {
			t.Fatal("a refused claim must not reach the skill sink")
			return RuntimeSkillRecord{}, nil
		}),
		contextSink,
	)

	for _, route := range []struct {
		resource string
		body     string
	}{
		{"skills", `{"name": "N", "instructions": "I"}`},
		{"project-context", `{"content": "C"}`},
	} {
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, builderRequest(t, route.resource, route.body))
		require.Equal(t, http.StatusForbidden, response.Code, route.resource)
	}
	require.Zero(t, contextSink.writeCallCount)
}

// ELITEA-2787's real content: an UPDATE that does not state `enabled` keeps the
// project's current value. The failure this pins is not hypothetical — a
// builder that defaulted it would switch a project's context off while editing
// its text, removing it from every prompt in the project.
func TestProjectContextWriteKeepsTheProjectsEnabledFlagWhenUnstated(t *testing.T) {
	t.Parallel()

	sink := &builderProjectContextSink{
		existing: RuntimeProjectContextRecord{Content: "old", Enabled: true, Found: true},
	}
	server := newBuilderTestServer(t, builderAuthorizerForProject(7), nil, sink)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "project-context", `{
		"content": "This project is a Python REST API, now with GraphQL."
	}`))

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, int64(7), sink.sawProjectID)
	require.Equal(t, "This project is a Python REST API, now with GraphQL.", sink.sawContent)
	require.True(t, sink.sawEnabled, "an update that did not state enabled must keep the stored value")

	var body RuntimeProjectContextWriteContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, RuntimeProjectContextWriteSchemaVersion, body.SchemaVersion)
	require.False(t, body.Created, "an existing context is updated, never duplicated")
	require.Equal(t, len(sink.sawContent), body.ContentBytes)
	// The stored text is deliberately NOT echoed: see
	// RuntimeProjectContextWriteContext's own doc comment.
	require.NotContains(t, response.Body.String(), "GraphQL")
}

// A project with no context yet gets one that is IN EFFECT. A context written
// from chat that is switched off is indistinguishable, from the user's side,
// from one that was never written at all.
func TestProjectContextWriteEnablesAFirstContextByDefault(t *testing.T) {
	t.Parallel()

	sink := &builderProjectContextSink{}
	server := newBuilderTestServer(t, builderAuthorizerForProject(7), nil, sink)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "project-context", `{"content": "First."}`))

	require.Equal(t, http.StatusOK, response.Code)
	require.True(t, sink.sawEnabled)

	var body RuntimeProjectContextWriteContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.True(t, body.Created)
}

// An explicit `false` still wins over the stored value — the pointer exists so
// "unstated" and "stated false" are different, and a test that only covered the
// unstated case would pass with the field ignored entirely.
func TestProjectContextWriteHonoursAnExplicitEnabledFalse(t *testing.T) {
	t.Parallel()

	sink := &builderProjectContextSink{
		existing: RuntimeProjectContextRecord{Content: "old", Enabled: true, Found: true},
	}
	server := newBuilderTestServer(t, builderAuthorizerForProject(7), nil, sink)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(
		t, "project-context", `{"content": "Draft.", "enabled": false}`,
	))
	require.Equal(t, http.StatusOK, response.Code)
	require.False(t, sink.sawEnabled)
}

// A sink failure is 503 and reports a NAMED stage, so an operator can tell a
// read failure from a write failure — the two mean different things about the
// database.
func TestProjectContextWriteReportsAStoreFailureAsUnavailable(t *testing.T) {
	t.Parallel()

	sink := &builderProjectContextSink{writeErr: errors.New("boom")}
	server := newBuilderTestServer(t, builderAuthorizerForProject(7), nil, sink)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "project-context", `{"content": "C"}`))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

// The routes must not exist at all where the service was not composed: a
// listener that answers 404 for a capability it does not serve is honest, while
// one that answers 503 forever claims the capability and never delivers it.
func TestBuilderRoutesAreAbsentWithoutTheService(t *testing.T) {
	t.Parallel()

	server := newBuilderTestServerWithoutBuilders(t)
	for _, resource := range []string{"skills", "project-context"} {
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, builderRequest(t, resource, `{"content": "C"}`))
		require.Equal(t, http.StatusNotFound, response.Code, resource)
	}
}

// A body over the cap is refused before it is read, never truncated: half an
// instruction set stored as though it were the whole one is silent corruption.
func TestBuilderRoutesRefuseAnOversizedBody(t *testing.T) {
	t.Parallel()

	server := newBuilderTestServer(
		t,
		builderAuthorizerForProject(7),
		builderSkillSinkFunc(func(
			context.Context, int64, string, string, string,
		) (RuntimeSkillRecord, error) {
			t.Fatal("an oversized body must not reach the sink")
			return RuntimeSkillRecord{}, nil
		}),
		&builderProjectContextSink{},
	)

	huge := `{"name":"N","instructions":"` + strings.Repeat("x", maxRuntimeBuilderRequestBytes+16) + `"}`
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "skills", huge))
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
}

func builderAuthorizerForProject(projectID int64) AgentRuntimeContextAuthorizer {
	return agentRuntimeContextAuthorizerFunc(func(
		context.Context, ContentClaim,
	) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{
			ResourceProjectID: projectID,
			ActorID:           "11",
			ConversationID:    "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1",
		}, nil
	})
}

func newBuilderTestServer(
	t *testing.T,
	authorizer AgentRuntimeContextAuthorizer,
	skills RuntimeSkillSink,
	projectContext RuntimeProjectContextSink,
) *ContentServer {
	t.Helper()
	if skills == nil {
		skills = builderSkillSinkFunc(func(
			context.Context, int64, string, string, string,
		) (RuntimeSkillRecord, error) {
			t.Fatal("this test must not reach the skill sink")
			return RuntimeSkillRecord{}, nil
		})
	}
	builders, err := NewRuntimeEntityBuilderService(authorizer, skills, projectContext)
	require.NoError(t, err)
	return newBuilderTestServerWithoutBuilders(t).WithRuntimeEntityBuilders(builders)
}

func newBuilderTestServerWithoutBuilders(t *testing.T) *ContentServer {
	t.Helper()
	server, err := NewMaterializingRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("a builder route must not call content-entry authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("a builder route must not open input content")
			return nil, nil
		}),
		contentMaterializerFunc(func(
			context.Context, ContentAuthorization, []byte, int64,
		) ([]byte, error) {
			t.Fatal("a builder route must not materialize input content")
			return nil, nil
		}),
		&EliteaClientTokenService{},
		1024,
		4,
	)
	require.NoError(t, err)
	return server
}

func builderRequest(t *testing.T, resource string, body string) *http.Request {
	t.Helper()
	path := "/executions/execution-1/generations/1/runtime-context/" + resource
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{&x509.Certificate{}}},
	}
	request.Header.Set(claimIDHeader, "claim-1")
	// 32 bytes: parseExecutionClaim requires a sha256-sized fence and answers
	// 401 for anything else, so a shorter literal would make every assertion
	// below pass or fail for the wrong reason.
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)))
	request.Header.Set("Content-Type", "application/json")
	return request
}
