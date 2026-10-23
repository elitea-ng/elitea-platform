package repos

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCurrentSDKValidationCandidateStagesNullConfigurationWithActualBundle(t *testing.T) {
	candidate := currentSDKValidationCandidateFixture()
	tenant := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{candidate.RevisionID, candidate.InputBundleID}}}}
	projects := &scriptedProjectStore{scriptedExecutor: tenant}
	repository, err := newCurrentSDKValidationCandidatesRepository(projects, &scriptedExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.StageCurrentSDKValidationCandidate(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if projects.projectID != int64(candidate.ProjectID) || len(tenant.rowCalls) != 1 {
		t.Fatalf("project=%d row calls=%d", projects.projectID, len(tenant.rowCalls))
	}
	call := tenant.rowCalls[0]
	if !strings.Contains(call.sql, "revision_id, configuration_id") || !strings.Contains(call.sql, "$1, NULL") {
		t.Fatalf("candidate is not explicitly detached from configuration row: %s", call.sql)
	}
	if call.args[0] != candidate.RevisionID || call.args[5] != candidate.InputBundleID {
		t.Fatalf("staged revision/bundle args = %#v", call.args)
	}
	if got := call.args[4].([]byte); string(got) != string(candidate.SettingsContentDigest[:]) {
		t.Fatal("staged settings digest changed")
	}
}

func TestCurrentSDKValidationCandidateObservationRequiresProjectionAndCommittedSettlement(t *testing.T) {
	tests := []struct {
		name   string
		row    []any
		status configurationapp.CurrentSDKValidationCandidateStatus
	}{
		{
			name:   "projection before settlement remains pending",
			row:    []any{"RUNNING", false, true, true, "", false, false},
			status: configurationapp.CurrentSDKValidationCandidatePending,
		},
		{
			name:   "valid projection and settlement",
			row:    []any{"SUCCEEDED", true, true, true, "SUCCEEDED", true, true},
			status: configurationapp.CurrentSDKValidationCandidateValid,
		},
		{
			name:   "invalid projection and settlement",
			row:    []any{"SUCCEEDED", true, true, false, "SUCCEEDED", true, true},
			status: configurationapp.CurrentSDKValidationCandidateInvalid,
		},
		{
			name:   "durably failed worker",
			row:    []any{"FAILED", true, false, false, "FAILED", true, false},
			status: configurationapp.CurrentSDKValidationCandidateFailed,
		},
		{
			name:   "durably cancelled worker",
			row:    []any{"CANCELLED", true, false, false, "CANCELLED", true, false},
			status: configurationapp.CurrentSDKValidationCandidateCancelled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tenant := &scriptedExecutor{rowResults: []scriptedRow{{values: test.row}}}
			projects := &scriptedProjectStore{scriptedExecutor: tenant}
			repository, err := newCurrentSDKValidationCandidatesRepository(projects, &scriptedExecutor{})
			if err != nil {
				t.Fatal(err)
			}
			execution := currentSDKValidationCandidateExecutionFixture()
			status, err := repository.ObserveCurrentSDKValidationCandidate(context.Background(), execution)
			if err != nil {
				t.Fatal(err)
			}
			if status != test.status {
				t.Fatalf("status = %s, want %s", status, test.status)
			}
			if projects.projectID != int64(execution.Candidate.ProjectID) || len(tenant.rowCalls) != 1 {
				t.Fatalf("project=%d row calls=%d", projects.projectID, len(tenant.rowCalls))
			}
			call := tenant.rowCalls[0]
			for _, fragment := range []string{
				"j.input_bundle_id = r.input_bundle_id",
				"b.manifest_digest = $12",
				"e.content_digest = r.settings_content_digest",
				"p.logical_output_id = s.final_logical_output_id",
				"r.configuration_id IS NULL",
				"r.created_by = $18",
			} {
				if !strings.Contains(call.sql, fragment) {
					t.Fatalf("observation is missing exact binding %q", fragment)
				}
			}
			if call.args[5] != execution.Candidate.InputBundleID || call.args[12] != execution.ExecutionID ||
				call.args[14] != execution.CommandID || call.args[17] != execution.Candidate.CreatedBy {
				t.Fatalf("observation identity args = %#v", call.args)
			}
		})
	}
}

func TestCurrentSDKValidationCandidateJanitorDeletesOnlyBoundOldTerminalRowsAndAuditsOrphans(t *testing.T) {
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tenant := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{int64(2)}},
		{values: []any{int64(1)}},
	}}
	projects := &scriptedProjectStore{scriptedExecutor: tenant}
	repository, err := newCurrentSDKValidationCandidatesRepository(projects, &scriptedExecutor{})
	if err != nil {
		t.Fatal(err)
	}

	result, err := repository.CleanupStaleCurrentSDKValidationCandidates(
		context.Background(),
		configurationapp.CurrentSDKValidationCleanupRequest{ProjectID: 7, OlderThan: cutoff, Limit: 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.TerminalDeleted != 2 || result.UnreferencedObserved != 1 {
		t.Fatalf("result = %+v", result)
	}
	if projects.projectID != 7 || len(tenant.rowCalls) != 2 {
		t.Fatalf("project=%d row calls=%d", projects.projectID, len(tenant.rowCalls))
	}

	cleanup := tenant.rowCalls[0]
	for _, fragment := range []string{
		"r.created_at <= $2",
		"LIMIT $3",
		"FOR UPDATE OF r SKIP LOCKED",
		"admitted.capability_id = 'configuration.validate.v1'",
		"admitted.input_bundle_id = r.input_bundle_id",
		"entry.content_digest = r.settings_content_digest",
		"settlement.committed_at <= $2",
		"admitted.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')",
		"referenced.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')",
		"projection.logical_output_id = settlement.final_logical_output_id",
		"DELETE FROM configuration_validation_projection",
		"DELETE FROM configuration_revisions",
	} {
		if !strings.Contains(cleanup.sql, fragment) {
			t.Fatalf("janitor cleanup is missing guard %q: %s", fragment, cleanup.sql)
		}
	}
	if cleanup.args[0] != int64(7) || cleanup.args[1] != cutoff || cleanup.args[2] != int32(3) {
		t.Fatalf("cleanup args = %#v", cleanup.args)
	}

	audit := tenant.rowCalls[1]
	if !strings.Contains(audit.sql, "NOT EXISTS (") || !strings.Contains(audit.sql, "LIMIT $2") ||
		strings.Contains(audit.sql, "DELETE FROM") {
		t.Fatalf("orphan audit must be bounded and read-only: %s", audit.sql)
	}
	if audit.args[0] != cutoff || audit.args[1] != int32(3) {
		t.Fatalf("audit args = %#v", audit.args)
	}
}

func TestCurrentSDKValidationCandidateJanitorRejectsUnboundedRequest(t *testing.T) {
	tenant := &scriptedExecutor{}
	repository, err := newCurrentSDKValidationCandidatesRepository(
		&scriptedProjectStore{scriptedExecutor: tenant},
		&scriptedExecutor{},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.CleanupStaleCurrentSDKValidationCandidates(
		context.Background(),
		configurationapp.CurrentSDKValidationCleanupRequest{
			ProjectID: 7,
			OlderThan: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			Limit:     configurationapp.MaxCurrentSDKValidationCleanupBatch + 1,
		},
	)
	if !errors.Is(err, configurationapp.ErrInvalidCurrentSDKValidationExecution) || len(tenant.rowCalls) != 0 {
		t.Fatalf("error=%v row calls=%d", err, len(tenant.rowCalls))
	}
}

func TestCurrentSDKValidationCandidateObservationMapsMissingExactBinding(t *testing.T) {
	tenant := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}}}
	repository, err := newCurrentSDKValidationCandidatesRepository(
		&scriptedProjectStore{scriptedExecutor: tenant},
		&scriptedExecutor{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ObserveCurrentSDKValidationCandidate(
		context.Background(),
		currentSDKValidationCandidateExecutionFixture(),
	)
	if !errors.Is(err, configurationapp.ErrCurrentSDKValidationCandidateNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestCurrentSDKValidationCandidateCancellationIsReferenceOnly(t *testing.T) {
	shared := &scriptedExecutor{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")}}
	repository, err := newCurrentSDKValidationCandidatesRepository(
		&scriptedProjectStore{scriptedExecutor: &scriptedExecutor{}},
		shared,
	)
	if err != nil {
		t.Fatal(err)
	}
	execution := currentSDKValidationCandidateExecutionFixture()
	if err := repository.RequestCurrentSDKValidationCancellation(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	if len(shared.execCalls) != 1 {
		t.Fatalf("exec calls = %d", len(shared.execCalls))
	}
	call := shared.execCalls[0]
	if strings.Contains(call.sql, "content_bytes") || strings.Contains(call.sql, "settings_content") ||
		!strings.Contains(call.sql, "SET desired_state = 'CANCELLED'") ||
		!strings.Contains(call.sql, "configuration_revision_id = $4") ||
		!strings.Contains(call.sql, "input_bundle_id = $5") {
		t.Fatalf("cancellation is not reference-only and exactly bound: %s", call.sql)
	}
	if call.args[0] != execution.ExecutionID || call.args[3] != execution.Candidate.RevisionID || call.args[4] != execution.Candidate.InputBundleID {
		t.Fatalf("cancellation args = %#v", call.args)
	}
}

func TestCurrentSDKValidationCandidateCleanupOnlyRemovesDetachedTerminalCandidate(t *testing.T) {
	tenant := &scriptedExecutor{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("DELETE 1")}}
	projects := &scriptedProjectStore{scriptedExecutor: tenant}
	repository, err := newCurrentSDKValidationCandidatesRepository(projects, &scriptedExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := currentSDKValidationCandidateFixture()
	if err := repository.CleanupCurrentSDKValidationCandidate(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if len(tenant.execCalls) != 1 || projects.projectID != int64(candidate.ProjectID) {
		t.Fatalf("exec calls=%d project=%d", len(tenant.execCalls), projects.projectID)
	}
	call := tenant.execCalls[0]
	for _, fragment := range []string{
		"r.configuration_id IS NULL",
		"EXISTS (",
		"NOT EXISTS (",
		"settlement.committed_at IS NOT NULL",
		"unsafe.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')",
		"projection.logical_output_id = settlement.final_logical_output_id",
		"DELETE FROM configuration_validation_projection",
		"DELETE FROM configuration_revisions",
	} {
		if !strings.Contains(call.sql, fragment) {
			t.Fatalf("cleanup is missing guard %q: %s", fragment, call.sql)
		}
	}
	if call.args[0] != candidate.RevisionID || call.args[1] != candidate.InputBundleID {
		t.Fatalf("cleanup args = %#v", call.args)
	}
}

func currentSDKValidationCandidateFixture() configurationapp.CurrentSDKValidationCandidate {
	return configurationapp.CurrentSDKValidationCandidate{
		ProjectID:             7,
		RevisionID:            "candidate-revision",
		ConfigurationType:     "github",
		SettingsEntryID:       "settings",
		SettingsEntryVersion:  "candidate-revision",
		SettingsContentDigest: runtimedomain.SHA256([]byte(`{"base_url":"https://api.github.com"}`)),
		InputBundleID:         "actual-input-bundle",
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		CatalogRevision:       "sdk-catalog-v1",
		CatalogDigest:         runtimedomain.SHA256([]byte("catalog")),
		SchemaID:              "elitea.configuration.github",
		SchemaRevision:        "schema-v1",
		SchemaDigest:          runtimedomain.SHA256([]byte("schema")),
		CreatedBy:             "13",
	}
}

func currentSDKValidationCandidateExecutionFixture() configurationapp.CurrentSDKValidationCandidateExecution {
	return configurationapp.CurrentSDKValidationCandidateExecution{
		Candidate:   currentSDKValidationCandidateFixture(),
		ExecutionID: "execution-1",
		CommandID:   "command-1",
		Generation:  1,
	}
}
