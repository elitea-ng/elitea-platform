package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type contentQueryerFunc func(context.Context, string, ...any) pgx.Row

func (f contentQueryerFunc) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return f(ctx, query, args...)
}

func TestPostgresNodeRecoveryContentIsOriginalInspectionWithoutMaterialization(t *testing.T) {
	data := []byte(`{"original":"frozen-input"}`)
	digest := sha256.Sum256(data)
	queries := 0
	repository, err := newPostgresContentRepository(contentQueryerFunc(func(_ context.Context, query string, args ...any) pgx.Row {
		queries++
		for _, guard := range []string{
			"c.released_at IS NULL", "c.lease_expires_at > clock_timestamp()", "ws.revoked_at IS NULL",
			"j.desired_state = 'RUNNING' AND c.recovery_mode <> 'NODE_RECOVERY'", "j.desired_state IN ('SUSPENDED', 'RUNNING') AND c.recovery_mode = 'NODE_RECOVERY'",
			"CASE WHEN c.recovery_mode = 'NODE_RECOVERY' THEN 'node.recovery.inspection'",
			"j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')",
			"AND e.semantic_role = 'agent.execution_request'", "e.required_grant_audience = $8",
		} {
			require.Contains(t, query, guard)
		}
		require.Len(t, args, 8)
		require.Equal(t, inputReadGrantAudience, args[7])
		return contentRowFunc(func(dest ...any) error {
			*dest[0].(*string) = "7"
			*dest[1].(*string) = "11"
			*dest[2].(*string) = "original-bundle"
			*dest[3].(*string) = "agent.execute.application.v1"
			*dest[4].(*string) = "node.recovery.inspection"
			*dest[5].(*string) = "application/json"
			*dest[6].(*[]byte) = digest[:]
			*dest[7].(*int64) = int64(len(data))
			return nil
		})
	}))
	require.NoError(t, err)
	server, err := NewMaterializingContentServerWithLimits(repository,
		contentStoreFunc(func(_ context.Context, project, bundle, _, _ string) (io.ReadCloser, error) {
			require.Equal(t, "7", project)
			require.Equal(t, "original-bundle", bundle)
			return io.NopCloser(bytes.NewReader(data)), nil
		}), contentMaterializerFunc(func(_ context.Context, _ ContentAuthorization, _ []byte, _ int64) ([]byte, error) {
			t.Fatal("paused original inspection redeemed credentials")
			return nil, nil
		}), 8192, 1)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	request := validContentRequest(t)
	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	request.TLS.VerifiedChains[0][0] = certificateWithURI(identity)
	server.Routes().ServeHTTP(response, request)
	require.Equal(t, 200, response.Code)
	require.Equal(t, data, response.Body.Bytes())
	require.Equal(t, 1, queries)
}

type contentRowFunc func(...any) error

func (f contentRowFunc) Scan(dest ...any) error {
	return f(dest...)
}

func certificateWithURI(identity *url.URL) *x509.Certificate {
	return &x509.Certificate{URIs: []*url.URL{identity}}
}

func TestPostgresContentAuthorizationRequiresInputReadAudience(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	wantDigest := sha256.Sum256([]byte(`{"auth_type":"Digest"}`))

	store := contentQueryerFunc(func(_ context.Context, query string, args ...any) pgx.Row {
		require.Contains(t, query, "j.actor_id")
		require.Contains(t, query, "e.required_grant_audience = $8")
		require.Contains(t, query, "ws.workload_session_id = c.workload_session_id")
		require.Contains(t, query, "ws.workload_identity = c.workload_identity")
		require.Contains(t, query, "ws.producer_id = c.producer_id")
		require.Contains(t, query, "ws.issued_at <= clock_timestamp()")
		require.Contains(t, query, "ws.expires_at > clock_timestamp()")
		require.Contains(t, query, "ws.revoked_at IS NULL")
		for _, capabilityID := range []string{
			"configuration.validate.v1",
			"index.ingest.v1",
			"agent.execute.application.v1",
			"agent.execute.adhoc.v1",
		} {
			require.Contains(t, query, "'"+capabilityID+"'")
		}
		require.Len(t, args, 8)
		require.Equal(t, inputReadGrantAudience, args[7])
		return contentRowFunc(func(dest ...any) error {
			require.Len(t, dest, 8)
			*dest[0].(*string) = "42"
			*dest[1].(*string) = "17"
			*dest[2].(*string) = "bundle-1"
			*dest[3].(*string) = "index.ingest.v1"
			*dest[4].(*string) = "index.toolkit_configuration"
			*dest[5].(*string) = "application/json"
			*dest[6].(*[]byte) = append([]byte(nil), wantDigest[:]...)
			*dest[7].(*int64) = 22
			return nil
		})
	})
	repository, err := newPostgresContentRepository(store)
	require.NoError(t, err)

	authorization, err := repository.AuthorizeContent(context.Background(), ContentClaim{
		PeerCertificate:  certificateWithURI(identity),
		ExecutionID:      "execution-1",
		Generation:       1,
		ClaimID:          "claim-1",
		FenceToken:       make([]byte, sha256.Size),
		ContentID:        "content-1",
		ImmutableVersion: "version-1",
	})
	require.NoError(t, err)
	require.Equal(t, "42", authorization.ResourceProjectID)
	require.Equal(t, "17", authorization.ActorID)
	require.Equal(t, "bundle-1", authorization.InputBundleID)
	require.Equal(t, "index.ingest.v1", authorization.CapabilityID)
	require.Equal(t, "index.toolkit_configuration", authorization.SemanticRole)
	require.Equal(t, "application/json", authorization.ExpectedMediaType)
	require.Equal(t, wantDigest, authorization.ExpectedDigest)
	require.EqualValues(t, 22, authorization.ExpectedLength)
}

func TestPostgresContentAuthorizationHidesAudienceMiss(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	repository, err := newPostgresContentRepository(contentQueryerFunc(
		func(_ context.Context, query string, args ...any) pgx.Row {
			require.True(t, strings.Contains(query, "e.required_grant_audience = $8"))
			require.Equal(t, inputReadGrantAudience, args[7])
			return contentRowFunc(func(...any) error { return pgx.ErrNoRows })
		},
	))
	require.NoError(t, err)

	_, err = repository.AuthorizeContent(context.Background(), ContentClaim{
		PeerCertificate:  certificateWithURI(identity),
		ExecutionID:      "execution-1",
		Generation:       1,
		ClaimID:          "claim-1",
		FenceToken:       make([]byte, sha256.Size),
		ContentID:        "content-1",
		ImmutableVersion: "version-1",
	})
	require.ErrorIs(t, err, ErrContentUnauthorized)
}
