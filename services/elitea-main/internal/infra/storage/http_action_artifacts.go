package storage

import (
	"context"
	"strings"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// NewHTTPActionTransport returns the only production HTTP action transport:
// the guard's RoundTripper (HTTPS only via egress.NewHTTPActionGuard, no
// proxy, no redirect followed, dial-time address check).
func NewHTTPActionTransport(guard *egress.Guard) (app.Transport, error) {
	if guard == nil {
		return nil, app.ErrUnavailable
	}
	return guard.RoundTripper(), nil
}

// HTTPActionArtifacts reads request bodies from, and writes large responses
// to, the project's artifact buckets as the execution's original actor. The
// bucket access list of that actor applies (RuntimeArtifactSource).
type HTTPActionArtifacts struct {
	source RuntimeArtifactSource
}

func NewHTTPActionArtifacts(source RuntimeArtifactSource) (*HTTPActionArtifacts, error) {
	if source == nil {
		return nil, app.ErrUnavailable
	}
	return &HTTPActionArtifacts{source: source}, nil
}

// Read accepts only a content-addressed reference (`artifact:<bucket>:<name>`
// whose immutable version is its SHA-256) and verifies the bytes it returns.
func (a *HTTPActionArtifacts) Read(ctx context.Context, admitted app.Admission, artifact app.Artifact, limit uint64) ([]byte, error) {
	bucket, name, ok := parseHTTPArtifactReference(artifact.Reference)
	if !ok || !app.ValidArtifact(artifact, limit) || artifact.ImmutableVersion != artifact.SHA256 {
		return nil, app.ErrUnauthorized
	}
	record, err := a.source.ReadRuntimeArtifact(ctx, admitted.ProjectID, admitted.ActorID, bucket, name, int64(limit))
	if err != nil || record.OverLimit || uint64(len(record.Content)) != artifact.ByteLength || app.Digest(record.Content) != artifact.SHA256 {
		return nil, app.ErrUnauthorized
	}
	return record.Content, nil
}

// Write stores one response under http-effects/<effect>/<sha256>.bin.
func (a *HTTPActionArtifacts) Write(ctx context.Context, admitted app.Admission, bucket, effectID, _ string, data []byte) (app.Artifact, error) {
	if bucket == "" || !app.ValidDigest(effectID) || len(data) == 0 || len(data) > app.MaxResponse {
		return app.Artifact{}, app.ErrInvalid
	}
	sum := app.Digest(data)
	name := "http-effects/" + effectID + "/" + sum + ".bin"
	if _, err := a.source.WriteRuntimeArtifact(ctx, admitted.ProjectID, admitted.ActorID, bucket, name, data); err != nil {
		return app.Artifact{}, app.ErrUnavailable
	}
	return app.Artifact{Reference: "artifact:" + bucket + ":" + name, ImmutableVersion: sum, SHA256: sum, ByteLength: uint64(len(data))}, nil
}

func parseHTTPArtifactReference(reference string) (string, string, bool) {
	rest, found := strings.CutPrefix(reference, "artifact:")
	if !found {
		return "", "", false
	}
	bucket, name, found := strings.Cut(rest, ":")
	if !found || bucket == "" || name == "" || len(bucket) > 128 || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return "", "", false
	}
	return bucket, name, true
}
