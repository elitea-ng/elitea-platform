package repos

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HTTPActionRepository owns the durable side of HTTP actions: the claim read
// (storage.HTTPActionSource), the live rule read (app.Rules) and the effect
// ledger (app.Effects: Lookup, Begin, Commit on execution_http_effects).
//
// Write budget per action: one INSERT (Begin) and one UPDATE (Commit). Claim,
// rule and configuration rows are read under FOR SHARE inside Begin, so a
// concurrent rule disable, configuration edit or permission removal is ordered
// before or after the effect row, never interleaved.
type HTTPActionRepository struct {
	store sharedStore
}

func NewHTTPActionRepository(pool *pgxpool.Pool) (*HTTPActionRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, app.ErrUnavailable
	}
	return &HTTPActionRepository{store: store}, nil
}

// ErrHTTPEffectFenced: Commit's claim or lease no longer owns the row; the row
// stays dispatching and the next Lookup reports it uncertain.
var ErrHTTPEffectFenced = errors.New("HTTP action effect commit is fenced")

// ReadHTTPActionInput reads the live claim and the frozen start input.
func (r *HTTPActionRepository) ReadHTTPActionInput(ctx context.Context, claim storage.ContentClaim) (storage.HTTPActionInput, error) {
	var input storage.HTTPActionInput
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		var err error
		input, err = readSavedChildClaim(ctx, tx, claim)
		return err
	})
	if err != nil {
		return storage.HTTPActionInput{}, app.ErrUnauthorized
	}
	return input, nil
}

// Rule reads the live rule a template names. A missing rule is policy_denied.
func (r *HTTPActionRepository) Rule(ctx context.Context, projectID int64, name string) (app.Rule, error) {
	rule, err := readHTTPActionRule(ctx, r.store, projectID, name, false)
	if err != nil {
		return app.Rule{}, httpPolicyDenied(err)
	}
	return rule, nil
}

type httpEffectRow struct {
	state, effect, inputsDigest, bindingDigest, requestDigest, claimID string
	expired                                                            bool
	receiptWire                                                        []byte
}

const httpEffectSelect = `SELECT state,effect_id,COALESCE(inputs_digest,''),frozen_binding_digest,request_digest,dispatch_claim_id,
 COALESCE(dispatch_deadline<clock_timestamp(),TRUE),receipt_wire
 FROM elitea_runtime.execution_http_effects
 WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND attempt=$4`

func readHTTPEffect(ctx context.Context, q sqlExecutor, admitted app.Admission, inv app.Invocation, lock bool) (httpEffectRow, bool, error) {
	statement := httpEffectSelect
	if lock {
		statement += " FOR UPDATE"
	}
	var row httpEffectRow
	err := q.QueryRow(ctx, statement, admitted.ExecutionID, int64(admitted.Generation), inv.ActivationID, int32(inv.Attempt)).Scan(&row.state, &row.effect, &row.inputsDigest, &row.bindingDigest, &row.requestDigest, &row.claimID, &row.expired, &row.receiptWire)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpEffectRow{}, false, nil
	}
	if err != nil {
		return httpEffectRow{}, false, app.ErrUnavailable
	}
	return row, true, nil
}

// Lookup returns the stored outcome of this (activation, attempt), if any.
func (r *HTTPActionRepository) Lookup(ctx context.Context, admitted app.Admission, inv app.Invocation) (app.Receipt, bool, error) {
	if !validHTTPAdmission(admitted) {
		return app.Receipt{}, false, app.ErrUnauthorized
	}
	var receipt app.Receipt
	var found bool
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		row, exists, err := readHTTPEffect(ctx, tx, admitted, inv, true)
		if err != nil || !exists {
			return err
		}
		receipt, err = settleExisting(ctx, tx, admitted, inv, row)
		found = err == nil
		return err
	})
	if err != nil {
		return app.Receipt{}, false, err
	}
	return receipt, found, nil
}

// settleExisting answers an existing row: a terminal receipt, in flight, or a
// stale dispatching row converted to uncertain/reconciliation_required. A
// dispatching row is never dispatched again.
func settleExisting(ctx context.Context, tx sqlExecutor, admitted app.Admission, inv app.Invocation, row httpEffectRow) (app.Receipt, error) {
	if row.inputsDigest != inv.InputsDigest || row.bindingDigest != inv.BindingDigest || row.effect != app.EffectID(admitted.ExecutionID, admitted.Generation, inv) {
		return app.Receipt{}, &app.Failure{Code: app.CodeNonDetermined}
	}
	switch row.state {
	case "completed", "failed", "uncertain":
		receipt, err := app.DecodeReceipt(row.receiptWire)
		if err != nil || receipt.EffectID != row.effect || receipt.State != row.state {
			return app.Receipt{}, app.ErrUnavailable
		}
		return receipt, nil
	case "dispatching":
		if row.claimID == admitted.ClaimID && !row.expired {
			return app.Receipt{}, app.ErrDispatchInFlight
		}
		failure := "reconciliation_required"
		receipt := app.Receipt{SchemaVersion: app.ReceiptSchema, ActivationID: inv.ActivationID, RequestDigest: row.requestDigest, BindingDigest: row.bindingDigest, EffectID: row.effect, State: "uncertain", FailureCode: &failure}
		wire, err := json.Marshal(receipt)
		if err != nil {
			return app.Receipt{}, app.ErrUnavailable
		}
		tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_http_effects
 SET state='uncertain',receipt_json=$5::jsonb,receipt_wire=$6,receipt_sha256=$7,resolved_at=clock_timestamp()
 WHERE execution_id=$1 AND generation=$2 AND effect_id=$3 AND state='dispatching' AND dispatch_claim_id=$4`,
			admitted.ExecutionID, int64(admitted.Generation), row.effect, row.claimID, string(wire), wire, app.Digest(wire))
		if err != nil || tag.RowsAffected() != 1 {
			return app.Receipt{}, app.ErrUnavailable
		}
		return receipt, nil
	default:
		return app.Receipt{}, app.ErrUnavailable
	}
}

// Begin persists the intent once, inside one transaction that rechecks every
// authority the dispatch depends on.
func (r *HTTPActionRepository) Begin(ctx context.Context, admitted app.Admission, inv app.Invocation, intent app.Intent) (app.Receipt, bool, error) {
	if !validHTTPAdmission(admitted) || intent.EffectID != app.EffectID(admitted.ExecutionID, admitted.Generation, inv) || len(intent.RequestWire) == 0 || len(intent.RequestWire) > app.MaxRequest || app.Digest(intent.RequestWire) != intent.RequestDigest || intent.Rule.ProjectID != admitted.ProjectID {
		return app.Receipt{}, false, app.ErrUnauthorized
	}
	var receipt app.Receipt
	dispatch := false
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		if err := lockHTTPActionClaim(ctx, tx, admitted); err != nil {
			return err
		}
		rule, err := readHTTPActionRule(ctx, tx, admitted.ProjectID, intent.Rule.Name, true)
		if err != nil || rule.ID != intent.Rule.ID || rule.Revision != intent.Rule.Revision || !rule.Enabled || rule.PolicyDigest() != intent.Rule.PolicyDigest() {
			return httpPolicyDenied(err)
		}
		if rule.ConfigurationID > 0 {
			access, err := httpConfigurationAccess(ctx, tx, admitted.ProjectID, admitted.ActorID, rule.ConfigurationID, true)
			if err != nil {
				return err
			}
			if access.revision != intent.CredentialRevision {
				return httpPolicyDenied(nil)
			}
		} else if intent.CredentialRevision != "" {
			return httpPolicyDenied(nil)
		}
		if inv.Attempt > 0 {
			previous := inv
			previous.Attempt--
			row, exists, err := readHTTPEffect(ctx, tx, admitted, previous, true)
			if err != nil {
				return err
			}
			if !exists || !intent.Retryable || row.state != "uncertain" && row.state != "failed" {
				return httpPolicyDenied(nil)
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_http_effects
 (execution_id,generation,activation_id,attempt,effect_id,request_digest,request_bytes,credential_revision,dispatch_claim_id,dispatch_lease_epoch,state,
  frozen_binding_digest,policy_digest,output_bucket,operation,inputs_digest,inputs_bytes,rule_id,rule_revision,rule_wire,dispatch_deadline)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'dispatching',$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
 ON CONFLICT DO NOTHING`,
			admitted.ExecutionID, int64(admitted.Generation), inv.ActivationID, int32(inv.Attempt), intent.EffectID, intent.RequestDigest, intent.RequestWire, intent.CredentialRevision, admitted.ClaimID, int64(admitted.LeaseEpoch),
			inv.BindingDigest, rule.PolicyDigest(), rule.OutputBucket, rule.Name, inv.InputsDigest, inv.Inputs, rule.ID, rule.Revision, rule.PolicyWire(), intent.DispatchDeadline)
		if err != nil {
			return app.ErrUnavailable
		}
		if tag.RowsAffected() == 1 {
			dispatch = true
			receipt = app.Receipt{SchemaVersion: app.ReceiptSchema, ActivationID: inv.ActivationID, RequestDigest: intent.RequestDigest, BindingDigest: inv.BindingDigest, EffectID: intent.EffectID, State: "dispatching"}
			return nil
		}
		row, exists, err := readHTTPEffect(ctx, tx, admitted, inv, true)
		if err != nil {
			return err
		}
		if !exists {
			// The effect id exists for another activation: never dispatch.
			return &app.Failure{Code: app.CodeNonDetermined}
		}
		receipt, err = settleExisting(ctx, tx, admitted, inv, row)
		return err
	})
	if err != nil {
		return app.Receipt{}, false, err
	}
	return receipt, dispatch, nil
}

// Commit records the outcome under the original dispatch claim and lease.
func (r *HTTPActionRepository) Commit(ctx context.Context, admitted app.Admission, receipt app.Receipt) error {
	if !validHTTPAdmission(admitted) || receipt.State != "completed" && receipt.State != "failed" && receipt.State != "uncertain" {
		return app.ErrUnauthorized
	}
	wire, err := json.Marshal(receipt)
	if err != nil || len(wire) > 3*1024*1024 {
		return app.ErrUnavailable
	}
	return r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		if err := lockHTTPActionClaim(ctx, tx, admitted); err != nil {
			return ErrHTTPEffectFenced
		}
		tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_http_effects
 SET state=$6,receipt_json=$7::jsonb,receipt_wire=$8,receipt_sha256=$9,resolved_at=clock_timestamp()
 WHERE execution_id=$1 AND generation=$2 AND effect_id=$3 AND state='dispatching' AND dispatch_claim_id=$4 AND dispatch_lease_epoch=$5`,
			admitted.ExecutionID, int64(admitted.Generation), receipt.EffectID, admitted.ClaimID, int64(admitted.LeaseEpoch), receipt.State, string(wire), wire, app.Digest(wire))
		if err != nil {
			return app.ErrUnavailable
		}
		if tag.RowsAffected() != 1 {
			return ErrHTTPEffectFenced
		}
		return nil
	})
}

// lockHTTPActionClaim fences a write on the live claim and lease epoch.
func lockHTTPActionClaim(ctx context.Context, tx sqlExecutor, admitted app.Admission) error {
	var claim string
	err := tx.QueryRow(ctx, `SELECT c.claim_id FROM elitea_runtime.execution_claims c
 JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 WHERE c.execution_id=$1 AND c.generation=$2 AND c.claim_id=$3 AND c.lease_epoch=$4
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
 AND j.desired_state='RUNNING' AND j.state IN ('RUNNING','SUSPENDED') AND j.resource_project_id=$5 AND j.actor_id=$6
 FOR SHARE OF c,j`, admitted.ExecutionID, int64(admitted.Generation), admitted.ClaimID, int64(admitted.LeaseEpoch), admitted.ProjectID, strconv.FormatInt(admitted.ActorID, 10)).Scan(&claim)
	if err != nil || claim != admitted.ClaimID {
		return app.ErrUnauthorized
	}
	return nil
}

func validHTTPAdmission(a app.Admission) bool {
	return a.ExecutionID != "" && len(a.ExecutionID) <= 256 && a.Generation > 0 && a.Generation <= math.MaxInt64 && a.ProjectID > 0 && a.ActorID > 0 && a.ClaimID != "" && a.LeaseEpoch > 0 && a.LeaseEpoch <= math.MaxInt64
}

// httpPolicyDenied keeps infrastructure failures retryable (503) and turns
// every authority refusal into policy_denied (403).
func httpPolicyDenied(err error) error {
	if err != nil && (errors.Is(err, app.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return app.ErrUnavailable
	}
	return &app.Failure{Code: app.CodePolicyDenied}
}
