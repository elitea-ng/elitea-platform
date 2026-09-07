package pipelinetriggers

// The three guards that cannot be reached through the HTTP surface, and one
// that must be asserted about the SOURCE because a passing behavioural test
// cannot tell a constant-time compare from a variable-time one.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// TestTheSecretCompareIsConstantTimeAndFixedWidth.
//
// A behavioural test cannot distinguish `subtle.ConstantTimeCompare` from
// `bytes.Equal`: both answer the same for every input, and a timing measurement
// in a unit test is noise. So the claim is made about the source, which is the
// only place the difference is visible — and about the WIDTH, which is what
// makes a constant-time compare constant time at all.
//
// This is the guard that would have caught the defect this package shipped and
// fixed during development: `newCredential` hashed the pre-encoding bytes while
// `secretDigest` hashed the request string, so every correct credential
// compared false. Both functions were individually correct.
func TestTheSecretCompareIsConstantTimeAndFixedWidth(t *testing.T) {
	source, err := os.ReadFile("inbound.go")
	if err != nil {
		t.Fatalf("read inbound.go: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, "subtle.ConstantTimeCompare(secretDigest(presented), trigger.TokenHash)") {
		t.Fatal("the inbound compare is no longer subtle.ConstantTimeCompare over the two digests. " +
			"An `==` or bytes.Equal here leaks the correct secret one byte at a time.")
	}
	// A variable-time compare of the two digests, in any of the shapes Go
	// offers. `presented == ""` is NOT on this list and must not be: refusing a
	// request that carried no credential at all is not a compare against a
	// stored secret.
	for _, forbidden := range []string{
		"bytes.Equal(",
		"string(trigger.TokenHash)",
		"presented == secret",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("inbound.go contains %q, which compares a credential in variable time", forbidden)
		}
	}

	// FIXED WIDTH. ConstantTimeCompare returns 0 for any length mismatch
	// regardless of content, so a digest of variable length would turn a
	// hashing change into "every token is wrong" rather than into a leak — and
	// the database CHECK on token_hash pins the same 32 bytes.
	for _, presented := range []string{"", "a", strings.Repeat("x", 4096), "not base64 at all !!"} {
		if got := len(secretDigest(presented)); got != 32 {
			t.Fatalf("secretDigest(%d bytes) is %d bytes, want 32", len(presented), got)
		}
	}
}

// TestMintedCredentialsVerifyAgainstTheirOwnDigest is the pairing the test
// above describes, asserted directly.
func TestMintedCredentialsVerifyAgainstTheirOwnDigest(t *testing.T) {
	for range 32 {
		tokenID, secret, hash, err := newCredential()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if len(tokenID) != tokenIDBytes*2 {
			t.Fatalf("token id is %d characters, want %d hex characters", len(tokenID), tokenIDBytes*2)
		}
		if secret == tokenID || strings.Contains(secret, tokenID) {
			t.Fatal("the secret is derived from the public token id")
		}
		if len(hash) != 32 {
			t.Fatalf("stored digest is %d bytes; the column CHECK requires 32", len(hash))
		}
		if string(secretDigest(secret)) != string(hash) {
			t.Fatal("a freshly minted secret does not hash to the digest stored beside it")
		}
	}
}

// TestVaultNamesAreUniquePerCredential. Two triggers sharing one vault entry
// would make the second save overwrite the first's secret, and the first would
// then authenticate with the wrong credential.
func TestVaultNamesAreUniquePerCredential(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		tokenID, _, _, err := newCredential()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		name := vaultSecretName(tokenID)
		if seen[name] {
			t.Fatalf("vault name %q was derived twice", name)
		}
		seen[name] = true
	}
}

/* ── the scheduled job ─────────────────────────────────────────────────── */

// TestScheduleJobRefusesAnOccurrenceThatIsNotItsOwn.
//
// The framework hands a handler a fenced occurrence. Running work for an
// occurrence whose job id, revision, lease epoch or claim fence does not match
// this registration means the handler was given somebody else's claim, and
// doing the work anyway is worse than refusing it. `index.schedule.scan.v1`
// validates the same six fields for the same reason.
func TestScheduleJobRefusesAnOccurrenceThatIsNotItsOwn(t *testing.T) {
	job, err := NewScheduleJob(NewHandler(&pgxpool.Pool{}))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if job.Name() != ScheduleJobID {
		t.Fatalf("Name() = %q, want %q", job.Name(), ScheduleJobID)
	}

	valid := schedulingapp.Occurrence{
		InvocationID:     "invocation-1",
		JobID:            ScheduleJobID,
		ScheduleRevision: ScheduleJobRevision,
		DueAt:            time.Now().UTC(),
		LeaseEpoch:       1,
		ClaimFence:       "fence-1",
	}
	for _, test := range []struct {
		name   string
		break_ func(schedulingapp.Occurrence) schedulingapp.Occurrence
	}{
		{"no invocation id", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.InvocationID = ""
			return o
		}},
		{"another job's occurrence", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.JobID = "index.schedule.scan.v1"
			return o
		}},
		{"a revision this build does not implement", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.ScheduleRevision = "pipeline-schedule-scan-r2"
			return o
		}},
		{"no due time", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.DueAt = time.Time{}
			return o
		}},
		{"no lease epoch", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.LeaseEpoch = 0
			return o
		}},
		{"no claim fence", func(o schedulingapp.Occurrence) schedulingapp.Occurrence {
			o.ClaimFence = ""
			return o
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := job.Execute(context.Background(), test.break_(valid)); err == nil {
				t.Fatal("the job accepted an occurrence that is not its own")
			}
		})
	}
}

// TestScheduleJobIsRegisteredAsDurableAdmission pins the mode against the
// framework's own validation, which is what refuses a handler returning the
// wrong outcome for its mode.
func TestScheduleJobIsRegisteredAsDurableAdmission(t *testing.T) {
	cadence, err := schedulingapp.ParseCron(ScheduleJobCadence)
	if err != nil {
		t.Fatalf("the registered cadence does not parse: %v", err)
	}
	job, err := NewScheduleJob(NewHandler(&pgxpool.Pool{}))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	// The lease the composition root uses. The framework refuses a job whose
	// timeout exceeds it, which is the check that stops a handler deadline from
	// knowingly outliving ownership of its occurrence.
	const leaseDuration = 2 * time.Minute
	if _, err := schedulingapp.NewRegistry(leaseDuration, schedulingapp.Job{
		ID:       ScheduleJobID,
		Revision: ScheduleJobRevision,
		Mode:     schedulingapp.ModeDurableAdmission,
		Schedule: cadence,
		Timeout:  ScheduleJobTimeout,
		Handler:  job,
	}); err != nil {
		t.Fatalf("the job this package registers is not a valid registration: %v", err)
	}
	if ScheduleJobTimeout >= leaseDuration {
		t.Fatalf("the handler timeout %s is not under the lease %s", ScheduleJobTimeout, leaseDuration)
	}
}

// TestNewPlatformHandlerDoesNotBoxATypedNil is the #86 trap, which this
// repository has now met at four composition roots: Go boxes a typed nil into a
// non-nil interface, so an absent dependency passed straight through makes
// every `!= nil` downstream read as "configured".
func TestNewPlatformHandlerDoesNotBoxATypedNil(t *testing.T) {
	var absentStart *fakeAbsentStart
	var absentVault *fakeAbsentVault
	handler := NewPlatformHandler(nil, absentStart, absentVault, nil, nil, nil)
	if handler.start != nil {
		t.Fatal("a nil start use case was boxed into a non-nil interface; the inbound route " +
			"would report a runtime it does not have and then dereference nil")
	}
	if handler.vault != nil {
		t.Fatal("a nil vault was boxed into a non-nil interface")
	}
	if _, err := handler.admit(context.Background(), "p_1", runRequest{}); err != ErrRuntimeUnavailable {
		t.Fatalf("admit with no runtime = %v, want ErrRuntimeUnavailable", err)
	}
}

type fakeAbsentStart struct{ AgentStartUseCase }

type fakeAbsentVault struct{ HiddenVault }
