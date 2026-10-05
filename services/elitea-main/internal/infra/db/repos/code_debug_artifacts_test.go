package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

// Adapt the shared SQL fixture without changing the production transaction contract.
type codeDebugTransactionFixture struct{ *scriptedExecutor }

func (f codeDebugTransactionFixture) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return f.scriptedExecutor.QueryRow(ctx, query, args...)
}

var _ storage.CodeTransaction = codeDebugTransactionFixture{}

// These tests exercise the actual debug repository transaction adapter. The
// owning original-visit PG/fence integration remains a separate required check.
type debugWriterLockStub struct {
	held, current bool
	locks, closes int
}

func TestCodeDebugReservationReadsExactVisitWhileRetainingRetryHistory(t *testing.T) {
	a := storage.CodeDebugAdmission{OriginalVisit: code.OriginalVisitRef{VisitID: strings.Repeat("1", 64), Revision: 1, DigestSHA256: strings.Repeat("2", 64)}, Attempt: 1, SchemaVersion: storage.CodeDebugAdmissionSchema, NodeID: "run", GraphThreadID: "thread", GraphStep: "2", ActivationID: strings.Repeat("a", 64), DefinitionSHA256: strings.Repeat("b", 64), YAMLSHA256: strings.Repeat("c", 64), ConfigurationJSON: `{"id":"run","type":"code","debug":true}`, RequestSHA256: strings.Repeat("d", 64), SourceSHA256: strings.Repeat("e", 64), InputSHA256: strings.Repeat("f", 64), SnapshotSHA256: strings.Repeat("3", 64), ByteLength: 10}
	raw, _ := json.Marshal(a)
	claim := storage.ContentClaim{ExecutionID: "execution-1", Generation: 7}
	auth := storage.CodeDebugAuthorization{TenantID: "7", ProjectID: 7, ActorID: 11, Language: "python"}
	repository := new(CodeDebugArtifactsRepository)
	key := strings.Repeat("4", 64) + ".json"
	for _, state := range []string{"staging", "committed"} {
		tx := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{raw, key, state, int64(11)}}}}
		upload, err := repository.readCodeDebugReservation(context.Background(), codeDebugTransactionFixture{tx}, claim, auth, a)
		if err != nil || upload.ObjectKey != key || upload.Admission.OriginalVisit != a.OriginalVisit {
			t.Fatal("same original visit failed to reconcile", err)
		}
		args := tx.rowCalls[0].args
		if len(args) != 7 || args[0] != "7" || args[1] != int64(7) || args[2] != claim.ExecutionID || args[3] != int64(7) || !bytes.Equal(args[4].([]byte), codeDebugBytes(a.OriginalVisit.VisitID)) || args[5] != int64(1) || !bytes.Equal(args[6].([]byte), codeDebugBytes(a.OriginalVisit.DigestSHA256)) {
			t.Fatal("reservation lookup did not bind complete immutable visit")
		}
	}
	retry := a
	retry.Attempt = 2
	retry.OriginalVisit.VisitID = strings.Repeat("5", 64)
	retry.OriginalVisit.DigestSHA256 = strings.Repeat("6", 64)
	tx := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{raw, key, "committed", int64(11)}}}}
	if _, err := repository.readCodeDebugReservation(context.Background(), codeDebugTransactionFixture{tx}, claim, auth, retry); !errors.Is(err, storage.ErrContentRejected) {
		t.Fatal("retry inherited previous publication grant")
	}
	retryRaw, _ := json.Marshal(retry)
	retryKey := strings.Repeat("7", 64) + ".json"
	tx = &scriptedExecutor{rowResults: []scriptedRow{{values: []any{retryRaw, retryKey, "staging", int64(11)}}}}
	upload, err := repository.readCodeDebugReservation(context.Background(), codeDebugTransactionFixture{tx}, claim, auth, retry)
	if err != nil || upload.ObjectKey == key || upload.Admission.Attempt != 2 {
		t.Fatal("separate retry publication collapsed into history", err)
	}
}

func (w *debugWriterLockStub) LockCurrentCodeDebugWriter(context.Context, storage.ContentClaim, storage.CodeDebugAdmission, storage.OriginalCodeVisit) (storage.CodeDebugAuthorization, func(), error) {
	if !w.current {
		return storage.CodeDebugAuthorization{}, nil, storage.ErrContentUnauthorized
	}
	w.held = true
	w.locks++
	return storage.CodeDebugAuthorization{ProjectID: 7, ActorID: 11}, func() { w.held = false; w.closes++ }, nil
}

type debugVisitTransactionStub struct {
	writer                                 *debugWriterLockStub
	current, completed, lockedAtCompletion bool
}

func (v *debugVisitTransactionStub) WithOriginalCodeVisit(ctx context.Context, _ storage.ContentClaim, _ code.OriginalVisitRef, purpose string, apply func(context.Context, storage.CodeTransaction, storage.OriginalCodeVisit) error) error {
	if purpose != "code_debug" || !v.current {
		return storage.ErrContentUnauthorized
	}
	err := apply(ctx, nil, storage.OriginalCodeVisit{})
	v.lockedAtCompletion = v.writer.held
	v.completed = true
	if err != nil {
		return err
	}
	if !v.current {
		return storage.ErrContentUnauthorized
	}
	return nil
}

func TestCodeDebugWriterLockSpansOnlyTheShortVisitTransaction(t *testing.T) {
	for _, callbackFailure := range []bool{false, true} {
		writer := &debugWriterLockStub{current: true}
		visit := &debugVisitTransactionStub{writer: writer, current: true}
		repository := &CodeDebugArtifactsRepository{visits: visit, writers: writer}
		sentinel := errors.New("metadata failure")
		err := repository.withCodeDebugVisit(context.Background(), storage.ContentClaim{}, storage.CodeDebugAdmission{}, func(context.Context, storage.CodeTransaction, storage.CodeDebugAuthorization) error {
			if !writer.held {
				t.Fatal("metadata callback ran without writer protection")
			}
			if callbackFailure {
				return sentinel
			}
			return nil
		})
		if (callbackFailure && !errors.Is(err, sentinel)) || (!callbackFailure && err != nil) || !visit.completed || !visit.lockedAtCompletion || writer.held || writer.locks != 1 || writer.closes != 1 {
			t.Fatal("writer lock ended before metadata completion or survived into IO", err)
		}
	}
}

func TestCodeDebugTakeoverRefusesBeforeReserveAfterIOAndAtCommit(t *testing.T) {
	for _, phase := range []string{"before-reserve", "after-external-io", "at-commit", "writer-before-reserve"} {
		t.Run(phase, func(t *testing.T) {
			writer := &debugWriterLockStub{current: phase != "writer-before-reserve"}
			visit := &debugVisitTransactionStub{writer: writer, current: phase != "before-reserve" && phase != "after-external-io"}
			repository := &CodeDebugArtifactsRepository{visits: visit, writers: writer}
			called := false
			err := repository.withCodeDebugVisit(context.Background(), storage.ContentClaim{}, storage.CodeDebugAdmission{}, func(context.Context, storage.CodeTransaction, storage.CodeDebugAuthorization) error {
				called = true
				if phase == "at-commit" {
					visit.current = false
				}
				return nil
			})
			if !errors.Is(err, storage.ErrContentUnauthorized) || writer.held {
				t.Fatal("takeover permitted publication or retained writer lock", err)
			}
			if phase != "at-commit" && called {
				t.Fatal("revoked authority ran metadata mutation")
			}
			if phase == "at-commit" && (!called || !visit.lockedAtCompletion || writer.closes != 1) {
				t.Fatal("final recheck lacked writer protection or cleanup")
			}
		})
	}
}
