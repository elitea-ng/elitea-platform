package execution

import (
	"errors"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestIndexRuntimeIdentityBindingPreservesAdmissionMode(t *testing.T) {
	job := indexRuntimeIdentityTestJob()
	for name, test := range map[string]struct {
		mode   IndexRuntimeAuthMode
		digest runtimedomain.Digest
	}{
		"actor PAT": {
			mode: IndexRuntimeAuthActorPAT,
		},
		"trusted session delegation": {
			mode:   IndexRuntimeAuthTrustedSessionDelegation,
			digest: runtimedomain.SHA256([]byte("server-side-short-lived-grant")),
		},
	} {
		t.Run(name, func(t *testing.T) {
			binding, err := NewIndexRuntimeIdentityBinding(job, test.mode, test.digest)
			if err != nil {
				t.Fatal(err)
			}
			if binding.Mode != test.mode ||
				binding.ExecutionID != job.ID ||
				binding.Generation != job.Generation ||
				binding.TenantID != job.TenantID ||
				binding.ResourceProjectID != job.ResourceProjectID ||
				binding.ActorID != job.ActorID {
				t.Fatalf("binding = %+v", binding)
			}
			if err := binding.ValidateAgainst(job); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndexRuntimeIdentityBindingRejectsCredentialModeMixing(t *testing.T) {
	job := indexRuntimeIdentityTestJob()
	grantDigest := runtimedomain.SHA256([]byte("server-side-short-lived-grant"))

	for name, test := range map[string]struct {
		mode   IndexRuntimeAuthMode
		digest runtimedomain.Digest
	}{
		"unknown mode": {
			mode: "fallback",
		},
		"actor PAT with delegation evidence": {
			mode:   IndexRuntimeAuthActorPAT,
			digest: grantDigest,
		},
		"trusted session without delegation evidence": {
			mode: IndexRuntimeAuthTrustedSessionDelegation,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewIndexRuntimeIdentityBinding(job, test.mode, test.digest)
			if !errors.Is(err, ErrInvalidIndexRuntimeIdentity) {
				t.Fatalf("error = %v, want ErrInvalidIndexRuntimeIdentity", err)
			}
		})
	}
}

func TestIndexRuntimeIdentityBindingRejectsReplayAndIdentityDrift(t *testing.T) {
	admitted := indexRuntimeIdentityTestJob()
	binding, err := NewIndexRuntimeIdentityBinding(
		admitted,
		IndexRuntimeAuthTrustedSessionDelegation,
		runtimedomain.SHA256([]byte("server-side-short-lived-grant")),
	)
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Job){
		"execution replay":  func(job *Job) { job.ID = "execution-other" },
		"generation replay": func(job *Job) { job.Generation++ },
		"cross tenant":      func(job *Job) { job.TenantID = "tenant-other" },
		"cross project":     func(job *Job) { job.ResourceProjectID = "43" },
		"cross actor":       func(job *Job) { job.ActorID = "8" },
		"wrong capability":  func(job *Job) { job.CapabilityID = ConfigurationValidationCapability },
	} {
		t.Run(name, func(t *testing.T) {
			runtimeJob := admitted
			mutate(&runtimeJob)
			if err := binding.ValidateAgainst(runtimeJob); !errors.Is(err, ErrInvalidIndexRuntimeIdentity) {
				t.Fatalf("error = %v, want ErrInvalidIndexRuntimeIdentity", err)
			}
		})
	}
}

func TestIndexRuntimeIdentityBindingRejectsMalformedIdentity(t *testing.T) {
	job := indexRuntimeIdentityTestJob()
	binding, err := NewIndexRuntimeIdentityBinding(job, IndexRuntimeAuthActorPAT, runtimedomain.Digest{})
	if err != nil {
		t.Fatal(err)
	}
	binding.SchemaVersion = "elitea.index.runtime-identity.future"
	if err := binding.ValidateAgainst(job); !errors.Is(err, ErrInvalidIndexRuntimeIdentity) {
		t.Fatalf("schema error = %v", err)
	}

	binding.SchemaVersion = IndexRuntimeIdentitySchemaVersion
	binding.ActorID = string([]byte{0xff})
	job.ActorID = binding.ActorID
	if err := binding.ValidateAgainst(job); !errors.Is(err, ErrInvalidIndexRuntimeIdentity) {
		t.Fatalf("actor error = %v", err)
	}
}

func indexRuntimeIdentityTestJob() Job {
	return Job{
		ID:                "execution-1",
		TenantID:          "tenant-1",
		ResourceProjectID: "42",
		ActorID:           "7",
		CapabilityID:      IndexIngestCapability,
		Generation:        3,
	}
}
