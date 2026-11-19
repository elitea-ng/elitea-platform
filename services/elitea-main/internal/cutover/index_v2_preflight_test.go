package cutover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type indexV1PersistedStateStub struct {
	state IndexV1PersistedState
	err   error
}

func (stub indexV1PersistedStateStub) ReadIndexV1CutoverState(context.Context) (IndexV1PersistedState, error) {
	return stub.state, stub.err
}

type indexControlStateStub struct {
	state IndexControlState
	err   error
}

func (stub indexControlStateStub) ReadIndexControlState(context.Context) (IndexControlState, error) {
	return stub.state, stub.err
}

func TestIndexV2PreflightRequiresEveryPersistedControlAndSpoolCountToBeZero(t *testing.T) {
	for _, test := range []struct {
		name      string
		persisted IndexV1PersistedState
		control   IndexControlState
		spoolFile bool
	}{
		{name: "ready"},
		{name: "live job", persisted: IndexV1PersistedState{LiveJobs: 1}},
		{name: "outstanding outbox", persisted: IndexV1PersistedState{OutstandingOutbox: 1}},
		{name: "active claim", persisted: IndexV1PersistedState{ActiveClaims: 1}},
		{name: "pending initialization", persisted: IndexV1PersistedState{PendingInitializations: 1}},
		{name: "pending terminal projection", persisted: IndexV1PersistedState{PendingTerminalProjections: 1}},
		{name: "pending manual cleanup", persisted: IndexV1PersistedState{PendingManualCleanups: 1}},
		{name: "stream reference", control: IndexControlState{StreamEntries: 1}},
		{name: "pending reference", control: IndexControlState{PendingEntries: 1}},
		{name: "delivery mapping", control: IndexControlState{DeliveryMappings: 1}},
		{name: "worker spool", spoolFile: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.spoolFile {
				if err := os.WriteFile(filepath.Join(root, "00000000000000000001.frame"), []byte("encrypted"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			preflight, err := NewIndexV2Preflight(
				indexV1PersistedStateStub{state: test.persisted},
				indexControlStateStub{state: test.control},
				[]string{root},
			)
			if err != nil {
				t.Fatal(err)
			}
			report, err := preflight.Check(context.Background())
			if test.name == "ready" {
				if err != nil || report.SpoolRoots != 1 || report.NonEmptySpoolDir != 0 {
					t.Fatalf("ready report=%+v err=%v", report, err)
				}
				return
			}
			if !errors.Is(err, ErrIndexV2CutoverBlocked) {
				t.Fatalf("blocked report=%+v err=%v", report, err)
			}
		})
	}
}

func TestIndexV2PreflightFailsClosedWhenPersistedStateCannotBeRead(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databaseFailure := errors.New("database unavailable")
	preflight, err := NewIndexV2Preflight(
		indexV1PersistedStateStub{err: databaseFailure},
		indexControlStateStub{},
		[]string{root},
	)
	if err != nil {
		t.Fatal(err)
	}

	report, err := preflight.Check(context.Background())
	if !errors.Is(err, databaseFailure) {
		t.Fatalf("database failure was not preserved: report=%+v err=%v", report, err)
	}
	if report != (IndexV2PreflightReport{}) {
		t.Fatalf("database failure returned partial report: %+v", report)
	}
}

func TestIndexV2PreflightFailsClosedOnUnsafeOrMissingSpoolCoverage(t *testing.T) {
	if _, err := NewIndexV2Preflight(
		indexV1PersistedStateStub{},
		indexControlStateStub{},
		nil,
	); err == nil {
		t.Fatal("missing spool coverage was accepted")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "spool")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	preflight, err := NewIndexV2Preflight(
		indexV1PersistedStateStub{},
		indexControlStateStub{},
		[]string{link},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preflight.Check(context.Background()); err == nil {
		t.Fatal("symlink spool root was accepted")
	}
}
