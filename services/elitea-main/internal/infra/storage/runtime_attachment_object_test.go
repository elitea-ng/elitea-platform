package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/extract"
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

	response := serve(server, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "chat-attachments", key,
	))
	require.Equal(t, codes.OK, response.Code, string(response.Body))
	require.Empty(t, response.Unreadable)

	require.EqualValues(t, 4242, sawProject, "the project must come from the claim")
	require.Equal(t, "chat-attachments", sawBucket)
	require.Equal(t, key, sawName)
	require.EqualValues(t, maxRuntimeAttachmentInputBytes, sawMaxBytes,
		"the source must be told the same ceiling the service enforces")

	var document RuntimeAttachmentObjectContext
	require.NoError(t, json.Unmarshal(response.Body, &document))
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

	response := serve(server, attachmentObjectRequest(
		t,
		certificateWithURI(identity),
		bytes.Repeat([]byte{7}, sha256.Size),
		"chat-attachments",
		attachmentTestConversation+"/report.txt",
	))
	require.Equal(t, codes.PermissionDenied, response.Code)
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
		var response contentResult
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
		response = serve(server, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size), "chat-attachments", key,
		))
		require.Contains(t,
			[]codes.Code{codes.PermissionDenied, codes.NotFound},
			response.Code,
			"key %q must not be served", key,
		)
	}
}

// TestAttachmentObjectRouteRefusesWhatItCannotServeAsText covers the outcomes
// that are neither a bad claim nor a missing file, and therefore must not be
// reported as either. Each is a successful call naming its reason, because
// the worker turns that reason into the sentence the model reads.
func TestAttachmentObjectRouteRefusesWhatItCannotServeAsText(t *testing.T) {
	t.Parallel()

	key := attachmentTestConversation + "/report.txt"
	oversized := bytes.Repeat([]byte("a"), maxRuntimeAttachmentInputBytes+1)
	for name, testCase := range map[string]struct {
		record AttachmentObjectRecord
		status codes.Code
		reason string
	}{
		// A source that ignored its own cap must still not get past the
		// service: the ceiling is enforced on both sides of the interface.
		"over the input cap": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: int64(len(oversized)), Content: oversized,
			},
			status: codes.OK, reason: "too_large",
		},
		"an image": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "image/png",
				ByteLength: 12, Content: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"),
			},
			status: codes.OK, reason: "unsupported_format",
		},
		"binary": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "application/octet-stream",
				ByteLength: 4, Content: []byte{0x00, 0x01, 0x02, 0x03},
			},
			status: codes.OK, reason: "unsupported_format",
		},
		"empty": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: 0, Content: []byte{},
			},
			status: codes.OK, reason: "empty",
		},
		// The metadata row and the bytes disagree: neither answer is
		// trustworthy, so neither is sent. It is a server condition (a read
		// that raced a re-upload), not "too large".
		"length disagrees with the bytes": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: key, MediaType: "text/plain",
				ByteLength: 99, Content: []byte("short"),
			},
			status: codes.Unavailable,
		},
		// A source that answered about a different object than was asked for.
		"identity does not echo the request": {
			record: AttachmentObjectRecord{
				Bucket: "chat-attachments", Name: attachmentTestConversation + "/other.txt",
				MediaType: "text/plain", ByteLength: 2, Content: []byte("hi"),
			},
			status: codes.NotFound,
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
			response := serve(server, attachmentObjectRequest(
				t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
				"chat-attachments", key,
			))
			require.Equal(t, testCase.status, response.Code, string(response.Body))
			require.Equal(t, testCase.reason, response.Unreadable)
		})
	}
}

// TestAttachmentObjectRouteMapsSourceFailuresToDistinctStatuses keeps the three
// meanings apart, which is the only thing that lets an operator tell a stale
// reference from a file this route will never be able to read.
func TestAttachmentObjectRouteMapsSourceFailuresToDistinctStatuses(t *testing.T) {
	t.Parallel()

	for status, failure := range map[codes.Code]error{
		codes.NotFound:    ErrContentNotFound,
		codes.OK:          ErrContentRejected, // served as the generic "unreadable" reason
		codes.Unavailable: io.ErrUnexpectedEOF,
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
		response := serve(server, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
			"chat-attachments", attachmentTestConversation+"/report.txt",
		))
		require.Equal(t, status, response.Code)
		if status == codes.OK {
			require.Equal(t, string(ReasonAttachmentUnreadable), response.Unreadable)
		}
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

	response := serve(server, func(server *ContentServer) contentResult {
		response, err := server.GetAttachmentObject(contentPeerContext(context.Background(), nil), &runtimev1.GetAttachmentObjectRequestV1{
			Scope: contentScope("execution-1", 1), Bucket: "chat-attachments", Name: attachmentTestConversation + "/report.txt",
		})
		return resultOf(response.GetDocumentJson(), err)
	})
	require.Equal(t, codes.Unauthenticated, response.Code)
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
		response := serve(server, attachmentObjectRequest(
			t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
			"chat-attachments", attachmentTestConversation+"/report.txt",
		))
		require.Equal(t, codes.Unavailable, response.Code,
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

	response := serve(server, attachmentObjectRequest(
		t, &x509.Certificate{}, bytes.Repeat([]byte{4}, sha256.Size),
		"chat-attachments", attachmentTestConversation+"/report.txt",
	))
	require.Equal(t, codes.Unimplemented, response.Code)
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

// attachmentObjectRequest prepares one GetAttachmentObject call. The key
// (`{conversationUUID}/{filename}`) travels as a field, slashes and all.
func attachmentObjectRequest(
	t *testing.T,
	certificate *x509.Certificate,
	fence []byte,
	bucket string,
	name string,
) contentCall {
	t.Helper()
	ctx := contentClaimContext(context.Background(), certificate, "claim-1", fence)
	return func(server *ContentServer) contentResult {
		response, err := server.GetAttachmentObject(ctx, &runtimev1.GetAttachmentObjectRequestV1{
			Scope:  contentScope("execution-1", 1),
			Bucket: bucket,
			Name:   name,
		})
		if err == nil && response.GetUnreadableReason() != "" {
			return contentResult{Code: codes.OK, Unreadable: response.GetUnreadableReason()}
		}
		return resultOf(response.GetDocumentJson(), err)
	}
}
