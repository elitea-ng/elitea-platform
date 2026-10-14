package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// artifactSourceStub records what the four routes ask for, so a test can prove
// that the PROJECT and the ACTOR reaching the source are the claim's and not
// anything the request carried.
type artifactSourceStub struct {
	projectID int64
	actorID   int64
	bucket    string
	name      string
	prefix    string
	recursive bool
	content   []byte

	listRecords []RuntimeArtifactRecord
	readRecord  RuntimeArtifactContentRecord
	writeRecord RuntimeArtifactRecord
	err         error
}

func (stub *artifactSourceStub) ListRuntimeArtifacts(
	_ context.Context, projectID, actorID int64, bucket, prefix string, recursive bool, _ int32,
) ([]RuntimeArtifactRecord, bool, error) {
	stub.projectID, stub.actorID, stub.bucket = projectID, actorID, bucket
	stub.prefix, stub.recursive = prefix, recursive
	return stub.listRecords, false, stub.err
}

func (stub *artifactSourceStub) ReadRuntimeArtifact(
	_ context.Context, projectID, actorID int64, bucket, name string, _ int64,
) (RuntimeArtifactContentRecord, error) {
	stub.projectID, stub.actorID, stub.bucket, stub.name = projectID, actorID, bucket, name
	return stub.readRecord, stub.err
}

func (stub *artifactSourceStub) WriteRuntimeArtifact(
	_ context.Context, projectID, actorID int64, bucket, name string, content []byte,
) (RuntimeArtifactRecord, error) {
	stub.projectID, stub.actorID, stub.bucket, stub.name = projectID, actorID, bucket, name
	stub.content = content
	return stub.writeRecord, stub.err
}

func (stub *artifactSourceStub) DeleteRuntimeArtifact(
	_ context.Context, projectID, actorID int64, bucket, name string,
) error {
	stub.projectID, stub.actorID, stub.bucket, stub.name = projectID, actorID, bucket, name
	return stub.err
}

// The four routes are ABSENT, not merely refusing, where the service is not
// composed — a deployment with no Go object store keeps the honest old
// behaviour (the worker skips the family) instead of one that always fails.
func TestArtifactRoutesAreAbsentWithoutTheService(t *testing.T) {
	t.Parallel()

	server := newBuilderTestServerWithoutBuilders(t)
	for _, operation := range []string{"list", "read", "write", "delete"} {
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, builderRequest(t, "artifacts/"+operation, `{}`))
		require.Equal(t, http.StatusNotFound, response.Code, operation)
	}
}

// Every route takes the project and the actor from the CLAIM. The request
// below names a bucket and nothing else; a route that let a body choose the
// project would be a cross-tenant read on a shared listener.
func TestArtifactRoutesActOnTheClaimsProjectAndActor(t *testing.T) {
	t.Parallel()

	source := &artifactSourceStub{
		listRecords: []RuntimeArtifactRecord{{
			Name: "reports/one.txt", MediaType: "text/plain", ByteLength: 4,
			ModifiedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		}},
		readRecord: RuntimeArtifactContentRecord{
			Name: "reports/one.txt", MediaType: "text/plain", ByteLength: 4, Content: []byte("data"),
		},
		writeRecord: RuntimeArtifactRecord{
			Name: "reports/one.txt", MediaType: "text/plain", ByteLength: 4,
		},
	}
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)

	listed := artifactCall[RuntimeArtifactListContext](
		t, server, "list", `{"bucket":"agent-artifacts","prefix":"reports/","recursive":true}`,
	)
	require.Equal(t, RuntimeArtifactListSchemaVersion, listed.SchemaVersion)
	require.Equal(t, int64(7), listed.ProjectID)
	require.Len(t, listed.Files, 1)
	require.Equal(t, "2026-09-20T10:00:00Z", listed.Files[0].ModifiedAt)
	require.Equal(t, int64(11), source.actorID)
	require.True(t, source.recursive)

	read := artifactCall[RuntimeArtifactReadContext](
		t, server, "read", `{"bucket":"agent-artifacts","name":"reports/one.txt"}`,
	)
	require.Equal(t, RuntimeArtifactReadSchemaVersion, read.SchemaVersion)
	require.Equal(t, "data", read.Content)
	require.False(t, read.OverLimit)
	require.Equal(t, int64(maxRuntimeArtifactReadChars), read.MaxChars)

	written := artifactCall[RuntimeArtifactWriteContext](
		t, server, "write", `{"bucket":"agent-artifacts","name":"reports/one.txt","content":"data"}`,
	)
	require.Equal(t, RuntimeArtifactWriteSchemaVersion, written.SchemaVersion)
	require.Equal(t, []byte("data"), source.content)

	deleted := artifactCall[RuntimeArtifactDeleteContext](
		t, server, "delete", `{"bucket":"agent-artifacts","name":"reports/one.txt"}`,
	)
	require.Equal(t, RuntimeArtifactDeleteSchemaVersion, deleted.SchemaVersion)
	require.True(t, deleted.Deleted)
	require.Equal(t, int64(7), source.projectID)
	require.Equal(t, int64(11), source.actorID)
}

// A file past the agent-path cap comes back as a MEASUREMENT with a 200, not
// as an error and not as a prefix: the tool has to be able to tell the model
// the cap and the actual size, and a 422 would carry neither.
func TestArtifactReadRefusesOverTheCapWithoutTruncating(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", maxRuntimeArtifactReadChars+10)
	source := &artifactSourceStub{readRecord: RuntimeArtifactContentRecord{
		Name: "big.txt", MediaType: "text/plain", ByteLength: int64(len(content)),
		Content: []byte(content),
	}}
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)

	read := artifactCall[RuntimeArtifactReadContext](
		t, server, "read", `{"bucket":"agent-artifacts","name":"big.txt"}`,
	)
	require.True(t, read.OverLimit)
	require.Empty(t, read.Content)
	require.Equal(t, int64(len(content)), read.CharLength)
	require.Equal(t, int64(maxRuntimeArtifactReadChars), read.MaxChars)
	require.Positive(t, read.TotalLines)
}

// A refused BUCKET is a 403 and a missing object a 404, on every route. The
// worker turns the first into "you may not touch that bucket" and the second
// into "that file is not there", which are different sentences for the model.
func TestArtifactRoutesMapTheSourceTaxonomy(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		err    error
		status int
	}{
		{"refused bucket", ErrContentUnauthorized, http.StatusForbidden},
		{"missing object", ErrContentNotFound, http.StatusNotFound},
		{"unreadable object", ErrContentRejected, http.StatusUnprocessableEntity},
		{"broken dependency", errors.New("boom"), http.StatusServiceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := newArtifactTestServer(
				t, builderAuthorizerForProject(7), &artifactSourceStub{err: testCase.err},
			)
			response := httptest.NewRecorder()
			server.Routes().ServeHTTP(response, builderRequest(
				t, "artifacts/read", `{"bucket":"agent-artifacts","name":"one.txt"}`,
			))
			require.Equal(t, testCase.status, response.Code)
		})
	}
}

// The write cap is enforced before anything is stored, and an unknown key
// fails loudly rather than being ignored — the same `DisallowUnknownFields`
// contract the builder writes have, so a version skew cannot half-apply.
func TestArtifactWriteRefusesOversizedAndUnknownBodies(t *testing.T) {
	t.Parallel()

	source := &artifactSourceStub{}
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)

	oversized := `{"bucket":"agent-artifacts","name":"big.txt","content":"` +
		strings.Repeat("x", maxRuntimeArtifactWriteChars+16) + `"}`
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "artifacts/write", oversized))
	require.Equal(t, http.StatusUnprocessableEntity, response.Code)

	response = httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(
		t, "artifacts/write", `{"bucket":"b","name":"n","content":"c","project_id":9}`,
	))
	require.Equal(t, http.StatusBadRequest, response.Code)

	require.Zero(t, source.projectID, "a refused body must not reach the artifact plane")
}

func newArtifactTestServer(
	t *testing.T,
	authorizer AgentRuntimeContextAuthorizer,
	source RuntimeArtifactSource,
) *ContentServer {
	t.Helper()
	artifacts, err := NewRuntimeArtifactObjectService(authorizer, source)
	require.NoError(t, err)
	return newBuilderTestServerWithoutBuilders(t).WithRuntimeArtifacts(artifacts)
}

func artifactCall[T any](t *testing.T, server *ContentServer, operation, body string) T {
	t.Helper()
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, builderRequest(t, "artifacts/"+operation, body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var decoded T
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded
}

// attachmentArtifactSourceStub also lists a conversation's attachments.
type attachmentArtifactSourceStub struct {
	artifactSourceStub
	policyBucket string
	records      []RuntimeArtifactRecord
	truncated    bool
	limit        int32
	listed       int
}

func (stub *attachmentArtifactSourceStub) AttachmentBucket(context.Context, int64) (string, error) {
	return stub.policyBucket, nil
}

func (stub *attachmentArtifactSourceStub) ListAttachmentsNewestFirst(
	_ context.Context, projectID, actorID int64, bucket, prefix string, limit int32,
) ([]RuntimeArtifactRecord, bool, error) {
	stub.projectID, stub.actorID, stub.bucket, stub.prefix, stub.limit = projectID, actorID, bucket, prefix, limit
	stub.listed++
	return stub.records, stub.truncated, nil
}

// An empty bucket is the conversation attachment listing: the project's
// attachment bucket (its policy, else the one default), the CLAIM's own
// conversation as the prefix whatever the request says (so a numeric id or a
// malformed one cannot send the worker to an empty listing), and an answer that
// names the bucket used.
func TestArtifactListWithoutABucketListsTheClaimsConversationInTheProjectsBucket(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ policy, want string }{
		{"team-files", "team-files"},
		{"", DefaultAttachmentBucket},
	} {
		source := &attachmentArtifactSourceStub{policyBucket: tc.policy}
		server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
		listed := artifactCall[RuntimeArtifactListContext](
			t, server, "list", `{"bucket":"","prefix":"42/","recursive":true,"limit":500}`,
		)
		require.Equal(t, tc.want, listed.Bucket)
		require.Equal(t, tc.want, source.bucket)
		require.Equal(t, "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1/", source.prefix,
			"the claim's conversation, not the request's prefix")
		require.Equal(t, int64(7), source.projectID)
		require.Equal(t, int64(11), source.actorID)
		require.Equal(t, int32(500), source.limit)
	}
}

// A project that has never had an upload has no attachment bucket yet: that is
// an empty listing, not a refusal.
func TestArtifactListOfAFreshProjectIsEmptyNotRefused(t *testing.T) {
	t.Parallel()

	source := &attachmentArtifactSourceStub{} // no records: the source found no bucket
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
	listed := artifactCall[RuntimeArtifactListContext](
		t, server, "list", `{"bucket":"","prefix":"","recursive":false,"limit":500}`,
	)
	require.Empty(t, listed.Files)
	require.False(t, listed.Truncated)
	require.Equal(t, DefaultAttachmentBucket, listed.Bucket)
}

// The order the source returns (newest upload first) is the order served, and
// the truncated flag it sets reaches the worker.
func TestArtifactListKeepsTheSourcesNewestFirstOrderAndTruncatedFlag(t *testing.T) {
	t.Parallel()

	source := &attachmentArtifactSourceStub{
		records: []RuntimeArtifactRecord{
			{Name: "c/new.txt", ByteLength: 1, ModifiedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
			{Name: "c/old.txt", ByteLength: 1, ModifiedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		},
		truncated: true,
	}
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
	listed := artifactCall[RuntimeArtifactListContext](
		t, server, "list", `{"bucket":"","prefix":"","recursive":false,"limit":500}`,
	)
	require.Equal(t, []string{"c/new.txt", "c/old.txt"}, []string{listed.Files[0].Name, listed.Files[1].Name})
	require.True(t, listed.Truncated)
}

// A source that cannot list attachments refuses a bucketless listing rather
// than guessing a name, and a bucketless READ is never resolved at all.
func TestArtifactListWithoutABucketIsRefusedWhenTheSourceCannotResolveIt(t *testing.T) {
	t.Parallel()

	server := newArtifactTestServer(t, builderAuthorizerForProject(7), &artifactSourceStub{})
	for _, call := range []struct{ operation, body string }{
		{"list", `{"bucket":"","prefix":"conv/"}`},
		{"read", `{"bucket":"","name":"conv/a.txt"}`},
	} {
		response := httptest.NewRecorder()
		server.Routes().ServeHTTP(response, builderRequest(t, "artifacts/"+call.operation, call.body))
		require.GreaterOrEqual(t, response.Code, 400, call.operation)
	}
}

// bucket_only asks for the bucket name alone: resolved exactly like the
// conversation listing (policy, else the default; the claim must carry a
// canonical conversation), but nothing is listed, so no keys are read or
// ordered, and a named bucket ignores it.
func TestArtifactListBucketOnlyResolvesTheBucketWithoutListing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ policy, want string }{
		{"team-files", "team-files"},
		{"", DefaultAttachmentBucket},
	} {
		source := &attachmentArtifactSourceStub{
			policyBucket: tc.policy,
			records:      []RuntimeArtifactRecord{{Name: "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1/a.csv"}},
		}
		server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
		listed := artifactCall[RuntimeArtifactListContext](
			t, server, "list", `{"bucket":"","prefix":"","recursive":false,"limit":1,"bucket_only":true}`,
		)
		require.Equal(t, tc.want, listed.Bucket)
		require.Equal(t, int64(7), listed.ProjectID)
		require.Empty(t, listed.Files)
		require.False(t, listed.Truncated)
		require.Zero(t, source.listed, "no listing is read for a bucket-only answer")
	}

	// Without the flag the same request still lists.
	source := &attachmentArtifactSourceStub{
		records: []RuntimeArtifactRecord{{Name: "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1/a.csv"}},
	}
	server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
	listed := artifactCall[RuntimeArtifactListContext](
		t, server, "list", `{"bucket":"","prefix":"","recursive":false,"limit":1}`,
	)
	require.Len(t, listed.Files, 1)
	require.Equal(t, 1, source.listed)

	// A named bucket ignores bucket_only: it is an ordinary listing there.
	named := &artifactSourceStub{}
	server = newArtifactTestServer(t, builderAuthorizerForProject(7), named)
	_ = artifactCall[RuntimeArtifactListContext](
		t, server, "list", `{"bucket":"reports","prefix":"","recursive":false,"limit":5,"bucket_only":true}`,
	)
	require.Equal(t, "reports", named.bucket)
}
