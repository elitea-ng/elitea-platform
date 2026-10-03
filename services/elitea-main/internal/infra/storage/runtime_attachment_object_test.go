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
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// attachmentTestConversation is the conversation every claim below resolves.
// Everything this route authorizes hangs off it, so it is written once and the
// tests differ only in which key they ASK for.
const attachmentTestConversation = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"

type attachmentObjectSourceFunc func(
	context.Context,
	int64,
	string,
	string,
	int64,
) (AttachmentObjectRecord, error)

func (f attachmentObjectSourceFunc) ReadAttachmentObject(
	ctx context.Context,
	projectID int64,
	bucket string,
	name string,
	maxBytes int64,
) (AttachmentObjectRecord, error) {
	return f(ctx, projectID, bucket, name, maxBytes)
}

// TestAttachmentObjectRouteServesTheClaimedConversationsFile is the happy path,
// and it asserts the two things the worker will not tolerate: the exact schema
// discriminator, and an identity that echoes back what was asked for.
//
// It also pins WHERE the project came from. The source is handed 4242 — the
// authorizer's resource_project_id — while the request carries no project at
// all, which is the whole shape of this route's tenancy.
func TestAttachmentObjectRouteServesTheClaimedConversationsFile(t *testing.T) {
	t.Parallel()

	key := attachmentTestConversation + "/report.txt"
	var sawProject int64
	var sawBucket, sawName string
	var sawMaxBytes int64
	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(
			_ context.Context, projectID int64, bucket, name string, maxBytes int64,
		) (AttachmentObjectRecord, error) {
			sawProject, sawBucket, sawName, sawMaxBytes = projectID, bucket, name, maxBytes
			return AttachmentObjectRecord{
				Bucket:     bucket,
				Name:       name,
				MediaType:  "text/plain",
				ByteLength: 11,
				Content:    []byte("hello there"),
			}, nil
		}),
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "chat-attachments", key,
	))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	require.EqualValues(t, 4242, sawProject, "the project must come from the claim")
	require.Equal(t, "chat-attachments", sawBucket)
	require.Equal(t, key, sawName)
	require.EqualValues(t, maxRuntimeAttachmentInputBytes, sawMaxBytes,
		"the source must be told the same ceiling the service enforces")

	var document RuntimeAttachmentObjectContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
	require.Equal(t, RuntimeAttachmentObjectSchemaVersion, document.SchemaVersion)
	require.EqualValues(t, 4242, document.ProjectID)
	require.Equal(t, "chat-attachments", document.Bucket)
	require.Equal(t, key, document.Name)
	require.Equal(t, "text/plain", document.MediaType)
	require.EqualValues(t, 11, document.ByteLength)
	require.Equal(t, "hello there", document.Content)
	require.Equal(t, "text", document.Format)
	require.Equal(t, extract.Version, document.ExtractorVersion)
	require.True(t, document.Complete)
	require.Equal(t, 1, document.UnitCount)
	require.Equal(t, []RuntimeAttachmentUnit{{Kind: "part", Label: "1", Start: 0, End: 11, HasText: true}}, document.Units)

	// The worker REJECTS a response whose cache policy is not exactly this
	// (validate_cache_policy in transport/runtime_context.rs), so a missing
	// header here is a route that answers 200 and is never read.
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	require.Contains(t, response.Header().Get("Cache-Control"), "no-cache")
	require.Equal(t, "no-cache", response.Header().Get("Pragma"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	digest := sha256.Sum256(response.Body.Bytes())
	require.Equal(t, formatSHA256Digest(digest), response.Header().Get("Content-Digest"))
}

// TestAttachmentObjectRouteRefusesAnIndexClaim is the capability gate, asserted
// against the REAL authorizer rather than a stub that says no.
//
// The stub version of this test would prove only that this file agrees with
// itself. What has to be true is that the query the service issues carries the
// agent-capability filter, so an index.ingest.v1 claim — which has a genuine
// resource_project_id and a live fence — finds no row and comes back 403 with
// the object never opened.
func TestAttachmentObjectRouteRefusesAnIndexClaim(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-index")
	require.NoError(t, err)

	var issued string
	repository, err := newPostgresContentRepository(contentQueryerFunc(
		func(_ context.Context, query string, _ ...any) pgx.Row {
			issued = query
			// What Postgres answers an index claim once the capability filter
			// is in the WHERE clause: the row is simply not visible.
			return contentRowFunc(func(...any) error { return pgx.ErrNoRows })
		},
	))
	require.NoError(t, err)

	server := newAttachmentObjectTestServer(
		t,
		repository,
		attachmentObjectSourceFunc(func(
			context.Context, int64, string, string, int64,
		) (AttachmentObjectRecord, error) {
			t.Fatal("an index claim must not reach object storage")
			return AttachmentObjectRecord{}, nil
		}),
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, attachmentObjectRequest(
		t,
		certificateWithURI(identity),
		bytes.Repeat([]byte{7}, sha256.Size),
		"chat-attachments",
		attachmentTestConversation+"/report.txt",
	))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, issued, agentRuntimeContextCapabilityFilter,
		"the attachment route must authorize through the agent-scoped capability filter")
	require.NotContains(t, issued, "'index.ingest.v1' AND i.execution_id IS NOT NULL) OR (j.capability_id = 'index.ingest.v1'")
}

// TestAttachmentObjectRouteRefusesACrossConversationObject is the reason this
// route can exist at all.
//
// The claim is perfectly valid — live, fenced, agent-scoped, and for a real
// project. It simply belongs to a DIFFERENT conversation than the object key
// names. Admission enforces exactly this prefix when it accepts the attachment
// (currentTurnAttachments), and without the same test here a live claim for one
// chat could read every attachment in the project into a model's context.
func TestAttachmentObjectRouteRefusesACrossConversationObject(t *testing.T) {
	t.Parallel()

	const otherConversation = "0e1f2a3b-4c5d-4e6f-8a9b-0c1d2e3f4a5b"
	for _, key := range []string{
		otherConversation + "/report.txt",
		// A prefix that merely CONTAINS the claim's conversation is not the
		// same as one keyed by it.
		otherConversation + "/" + attachmentTestConversation + "/report.txt",
		// The bare prefix addresses no object; admitting it would reduce the
		// test to "starts with a uuid".
		attachmentTestConversation + "/",
		// A traversal back out of the conversation's own prefix.
		attachmentTestConversation + "/../" + otherConversation + "/report.txt",
	} {
		response := httptest.NewRecorder()
		server := newAttachmentObjectTestServer(
			t,
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			attachmentObjectSourceFunc(func(
				context.Context, int64, string, string, int64,
			) (AttachmentObjectRecord, error) {
				t.Fatalf("a key outside the claim's conversation must not be opened: %q", key)
				return AttachmentObjectRecord{}, nil
			}),
		)
		server.Routes().ServeHTTP(response, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "chat-attachments", key,
		))
		require.Contains(t,
			[]int{http.StatusForbidden, http.StatusNotFound},
			response.Code,
			"key %q must not be served", key,
		)
	}
}

// TestAttachmentObjectRouteRefusesWhatItCannotServeAsText covers the outcomes
// that are neither a bad claim nor a missing file, and therefore must not be
// reported as either. Each 422 names its reason in the body, because the
// worker turns that reason into the sentence the model reads.
func TestAttachmentObjectRouteRefusesWhatItCannotServeAsText(t *testing.T) {
	t.Parallel()

	key := attachmentTestConversation + "/report.txt"
	oversized := bytes.Repeat([]byte("a"), maxRuntimeAttachmentInputBytes+1)
	for name, testCase := range map[string]struct {
		record AttachmentObjectRecord
		status int
		reason string
	}{
		// A source that ignored its own cap must still not get past the
		// service: the ceiling is enforced on both sides of the interface.
		"over the input cap": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: int64(len(oversized)), Content: oversized,
			},
			status: http.StatusUnprocessableEntity, reason: "too_large",
		},
		"an image": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "image/png",
				ByteLength: 12, Content: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"),
			},
			status: http.StatusUnprocessableEntity, reason: "unsupported_format",
		},
		"binary": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "application/octet-stream",
				ByteLength: 4, Content: []byte{0x00, 0x01, 0x02, 0x03},
			},
			status: http.StatusUnprocessableEntity, reason: "unsupported_format",
		},
		"empty": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: 0, Content: []byte{},
			},
			status: http.StatusUnprocessableEntity, reason: "empty",
		},
		// The metadata row and the bytes disagree: neither answer is
		// trustworthy, so neither is sent. It is a server condition (a read
		// that raced a re-upload), not "too large".
		"length disagrees with the bytes": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: 99, Content: []byte("short"),
			},
			status: http.StatusServiceUnavailable,
		},
		// A source that answered about a different object than was asked for.
		"identity does not echo the request": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: attachmentTestConversation + "/other.txt",
				MediaType: "text/plain", ByteLength: 2, Content: []byte("hi"),
			},
			status: http.StatusNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newAttachmentObjectTestServer(
				t,
				attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
				attachmentObjectSourceFunc(func(
					context.Context, int64, string, string, int64,
				) (AttachmentObjectRecord, error) {
					return testCase.record, nil
				}),
			)
			response := httptest.NewRecorder()
			server.Routes().ServeHTTP(response, attachmentObjectRequest(
				t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
				"chat-attachments", key,
			))
			require.Equal(t, testCase.status, response.Code, response.Body.String())
			if testCase.reason != "" {
				var body RuntimeAttachmentUnreadable
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.Equal(t, RuntimeAttachmentUnreadableSchemaVersion, body.SchemaVersion)
				require.Equal(t, testCase.reason, body.Reason)
				require.Equal(t, "application/json", response.Header().Get("Content-Type"))
			}
		})
	}
}

// TestAttachmentObjectRouteMapsSourceFailuresToDistinctStatuses keeps the three
// meanings apart, which is the only thing that lets an operator tell a stale
// reference from a file this route will never be able to read.
func TestAttachmentObjectRouteMapsSourceFailuresToDistinctStatuses(t *testing.T) {
	t.Parallel()

	for status, failure := range map[int]error{
		http.StatusNotFound:            ErrContentNotFound,
		http.StatusUnprocessableEntity: ErrContentRejected,
		http.StatusServiceUnavailable:  io.ErrUnexpectedEOF,
	} {
		server := newAttachmentObjectTestServer(
			t,
			attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
			attachmentObjectSourceFunc(func(
				context.Context, int64, string, string, int64,
			) (AttachmentObjectRecord, error) {
				return AttachmentObjectRecord{}, failure
			}),
		)
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
			"chat-attachments", attachmentTestConversation+"/report.txt",
		))
		require.Equal(t, status, response.Code)
	}
}

// TestAttachmentObjectRouteRequiresTheClaimBeforeAnythingElse pins the ORDER.
// An unauthenticated request must be refused before the reference is even
// looked at, so a caller cannot use this route to probe which objects exist.
func TestAttachmentObjectRouteRequiresTheClaimBeforeAnythingElse(t *testing.T) {
	t.Parallel()

	server := newAttachmentObjectTestServer(
		t,
		attachmentTestAuthorizer(t, 4242, attachmentTestConversation),
		attachmentObjectSourceFunc(func(
			context.Context, int64, string, string, int64,
		) (AttachmentObjectRecord, error) {
			t.Fatal("an unauthenticated request must not reach object storage")
			return AttachmentObjectRecord{}, nil
		}),
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/executions/execution-1/generations/1/runtime-context/attachments/chat-attachments/"+
			url.PathEscape(attachmentTestConversation+"/report.txt"),
		nil,
	))
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

// TestAttachmentObjectRouteRefusesAConversationlessClaim covers the claim shape
// this route cannot authorize on: an agent capability that somehow resolved no
// conversation. It is a 503 with a named stage, not a 403, because the row's
// client_stream_id is NOT NULL — an empty one means this service and the
// admission path disagree, which an operator has to see.
func TestAttachmentObjectRouteRefusesAConversationlessClaim(t *testing.T) {
	t.Parallel()

	for _, conversation := range []string{"", "1", strings.ToUpper(attachmentTestConversation)} {
		server := newAttachmentObjectTestServer(
			t,
			attachmentTestAuthorizer(t, 4242, conversation),
			attachmentObjectSourceFunc(func(
				context.Context, int64, string, string, int64,
			) (AttachmentObjectRecord, error) {
				t.Fatal("a claim with no canonical conversation must not open an object")
				return AttachmentObjectRecord{}, nil
			}),
		)
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
			"chat-attachments", attachmentTestConversation+"/report.txt",
		))
		require.Equal(t, http.StatusServiceUnavailable, response.Code,
			"conversation %q", conversation)
	}
}

// TestAttachmentObjectRouteIsAbsentWithoutItsService is the composition claim:
// a listener built without object storage has no attachment route at all,
// rather than one that answers 503 forever.
func TestAttachmentObjectRouteIsAbsentWithoutItsService(t *testing.T) {
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
	require.Nil(t, server.runtimeObjects)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
		"chat-attachments", attachmentTestConversation+"/report.txt",
	))
	require.Equal(t, http.StatusNotFound, response.Code)
}

// TestAttachmentObjectServiceRequiresItsDependencies keeps a half-wired service
// from being constructible: a nil authorizer here would be the
// principal-validation bypass this codebase has already paid for repeatedly.
func TestAttachmentObjectServiceRequiresItsDependencies(t *testing.T) {
	t.Parallel()

	extractor := extract.New(extract.DefaultLimits())
	_, err := NewRuntimeAttachmentObjectService(nil, attachmentObjectSourceFunc(nil), extractor, nil)
	require.Error(t, err)
	_, err = NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 1, attachmentTestConversation), nil, extractor, nil,
	)
	require.Error(t, err)
	_, err = NewRuntimeAttachmentObjectService(
		attachmentTestAuthorizer(t, 1, attachmentTestConversation), attachmentObjectSourceFunc(nil), nil, nil,
	)
	require.Error(t, err)

	_, err = NewAgentAttachmentRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return nil, nil
		}),
		contentMaterializerFunc(func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error) {
			return nil, nil
		}),
		&EliteaClientTokenService{},
		&RuntimeApplicationVersionService{},
		nil,
		1024,
		1,
	)
	require.Error(t, err)
}

func attachmentTestAuthorizer(
	t *testing.T,
	projectID int64,
	conversationID string,
) AgentRuntimeContextAuthorizer {
	t.Helper()
	return agentRuntimeContextAuthorizerFunc(func(
		_ context.Context, claim ContentClaim,
	) (RuntimeContextAuthorization, error) {
		require.Equal(t, "execution-1", claim.ExecutionID)
		require.EqualValues(t, 1, claim.Generation)
		return RuntimeContextAuthorization{
			ResourceProjectID: projectID,
			ActorID:           "17",
			Initiator:         runtimeContextInitiatorUser,
			ConversationID:    conversationID,
		}, nil
	})
}

func newAttachmentObjectTestServer(
	t *testing.T,
	authorizer AgentRuntimeContextAuthorizer,
	source AttachmentObjectSource,
) *ContentServer {
	t.Helper()
	objects, err := NewRuntimeAttachmentObjectService(
		authorizer, source, extract.New(extract.DefaultLimits()), nil,
	)
	require.NoError(t, err)
	return newAttachmentObjectTestServerWithService(t, objects)
}

func newAttachmentObjectTestServerWithService(
	t *testing.T,
	objects *RuntimeAttachmentObjectService,
) *ContentServer {
	t.Helper()
	server, err := NewAgentAttachmentRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("the attachment route must not call content-entry authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("the attachment route must not open input content")
			return nil, nil
		}),
		contentMaterializerFunc(func(
			context.Context, ContentAuthorization, []byte, int64,
		) ([]byte, error) {
			t.Fatal("the attachment route must not materialize input content")
			return nil, nil
		}),
		&EliteaClientTokenService{},
		&RuntimeApplicationVersionService{},
		objects,
		1024,
		1,
	)
	require.NoError(t, err)
	return server
}

// attachmentObjectRequest percent-encodes the KEY into one path segment, which
// is how the worker addresses it: the key contains slashes
// (`{conversationUUID}/{filename}`) and chi routes on r.URL.RawPath, so `%2F`
// stays inside {name} instead of splitting into extra segments.
func attachmentObjectRequest(
	t *testing.T,
	certificate *x509.Certificate,
	fence []byte,
	bucket string,
	name string,
) *http.Request {
	t.Helper()
	path := strings.Join([]string{
		"/executions/execution-1/generations/1/runtime-context/attachments",
		url.PathEscape(bucket),
		strings.ReplaceAll(url.PathEscape(name), "/", "%2F"),
	}, "/")
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}
	request.Header.Set(claimIDHeader, "claim-1")
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(fence))
	return request
}
