package output_test

import (
	"errors"
	"testing"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func validIndexArtifactGrantRequest() outputapp.CreateArtifactGrantRequest {
	return outputapp.CreateArtifactGrantRequest{
		TenantID:            "tenant-a",
		ResourceProjectID:   "1",
		ProjectionProjectID: "1",
		CommandID:           "command-a",
		ExecutionID:         "execution-a",
		Generation:          1,
		Artifact: outputapp.IndexArtifactReference{
			ArtifactID:       "artifact-a",
			ImmutableVersion: "v1",
			MediaType:        "application/json",
			ByteLength:       128,
			Digest:           runtimedomain.SHA256([]byte("index artifact grant payload")),
			Classification:   "project-confidential",
		},
	}
}

// TestIndexIngestArtifactCreateGrantRequestValidationRejectsMissingIdentity
// proves CreateArtifactGrantRequest.Validate rejects every missing identity
// field independently — a producer that forgets one of these has no other
// gate catching it before CreateArtifactGrant would otherwise mint a grant
// CommitArtifact could never durably settle (elitea_runtime.index_result_
// artifacts' own NOT NULL/FK constraints would only surface the mistake at
// commit time, as an opaque database error).
func TestIndexIngestArtifactCreateGrantRequestValidationRejectsMissingIdentity(t *testing.T) {
	base := validIndexArtifactGrantRequest()
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline request must validate cleanly: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*outputapp.CreateArtifactGrantRequest)
	}{
		{"missing tenant", func(r *outputapp.CreateArtifactGrantRequest) { r.TenantID = "" }},
		{"missing resource project", func(r *outputapp.CreateArtifactGrantRequest) { r.ResourceProjectID = "" }},
		{"missing projection project", func(r *outputapp.CreateArtifactGrantRequest) { r.ProjectionProjectID = "" }},
		{"missing command", func(r *outputapp.CreateArtifactGrantRequest) { r.CommandID = "" }},
		{"missing execution", func(r *outputapp.CreateArtifactGrantRequest) { r.ExecutionID = "" }},
		{"zero generation", func(r *outputapp.CreateArtifactGrantRequest) { r.Generation = 0 }},
		{"empty artifact id", func(r *outputapp.CreateArtifactGrantRequest) { r.Artifact.ArtifactID = "" }},
		{"zero byte length", func(r *outputapp.CreateArtifactGrantRequest) { r.Artifact.ByteLength = 0 }},
		{"zero digest", func(r *outputapp.CreateArtifactGrantRequest) { r.Artifact.Digest = runtimedomain.Digest{} }},
		{"invalid media type", func(r *outputapp.CreateArtifactGrantRequest) { r.Artifact.MediaType = "not/a/media/type" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validIndexArtifactGrantRequest()
			tc.mutate(&req)
			if err := req.Validate(); !errors.Is(err, outputapp.ErrInvalidIndexIngestOutput) {
				t.Fatalf("Validate() = %v, want ErrInvalidIndexIngestOutput", err)
			}
		})
	}
}

// TestIndexIngestArtifactCommitGrantRequestValidationRequiresGrantID proves
// CommitArtifactRequest layers its own GrantID check on top of the embedded
// CreateArtifactGrantRequest's — a caller cannot commit without naming
// which grant it uploaded against, independent of whether the rest of the
// identity/reference is otherwise well-formed.
func TestIndexIngestArtifactCommitGrantRequestValidationRequiresGrantID(t *testing.T) {
	req := outputapp.CommitArtifactRequest{
		GrantID:                    "",
		CreateArtifactGrantRequest: validIndexArtifactGrantRequest(),
	}
	if err := req.Validate(); !errors.Is(err, outputapp.ErrInvalidIndexIngestOutput) {
		t.Fatalf("Validate() with empty GrantID = %v, want ErrInvalidIndexIngestOutput", err)
	}

	req.GrantID = "11111111-1111-4111-8111-111111111111"
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() with a GrantID and an otherwise-valid embedded request = %v, want nil", err)
	}

	req.TenantID = ""
	if err := req.Validate(); !errors.Is(err, outputapp.ErrInvalidIndexIngestOutput) {
		t.Fatalf("Validate() must still enforce the embedded CreateArtifactGrantRequest's own rules: got %v", err)
	}
}

// TestIndexIngestArtifactGrantConflictIsDistinctFromMismatch proves
// ErrArtifactGrantConflict (a *different* artifact identity durably
// committed under the same ArtifactID/ImmutableVersion) is a distinct
// sentinel from ErrIndexIngestArtifactMismatch (the uploaded bytes
// disagree with the grant that was created for them) — a caller
// distinguishing "retry with a fresh grant landed on someone else's
// artifact identity" from "the upload itself didn't match" needs these to
// never collapse into the same errors.Is check.
func TestIndexIngestArtifactGrantConflictIsDistinctFromMismatch(t *testing.T) {
	if errors.Is(outputapp.ErrArtifactGrantConflict, outputapp.ErrIndexIngestArtifactMismatch) {
		t.Fatalf("ErrArtifactGrantConflict must not satisfy errors.Is against ErrIndexIngestArtifactMismatch")
	}
	if errors.Is(outputapp.ErrArtifactGrantConflict, outputapp.ErrIndexIngestArtifactUnavailable) {
		t.Fatalf("ErrArtifactGrantConflict must not satisfy errors.Is against ErrIndexIngestArtifactUnavailable")
	}
}
