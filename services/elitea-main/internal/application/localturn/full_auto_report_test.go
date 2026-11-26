package localturn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCommitStoresFullAuto pins the ADR-0061 amendment of 2026-11-26 (issue
// 3079): a Full auto turn's record is stored on `local_work.full_auto`,
// bounded (empty commands dropped, the count never below the kept answered
// asks: a held one is not answered), and a
// report without it stores none, so its digest is unchanged.
func TestCommitStoresFullAuto(t *testing.T) {
	store := &commitFakeStore{}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := hostReportCommit()
	asks := []FullAutoAsk{
		{Command: "run_command: gh pr view 12", Reason: "after reading content from outside this folder", Tainted: true},
		{Command: "  ", Reason: "dropped"},
		{Command: "run_command: cat ~/.ssh/id", Reason: "credential store", Held: true},
	}
	for range MaxReportedCommands {
		asks = append(asks, FullAutoAsk{Command: "run_command: git fetch " + strings.Repeat("x", 400), Reason: "network"})
	}
	request.Report.FullAuto = &FullAutoReport{Answered: 1, Asks: asks}
	if _, err := service.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		LocalWork struct {
			FullAuto *FullAutoReport `json:"full_auto"`
		} `json:"local_work"`
	}
	if err := json.Unmarshal(store.last.ResponseMeta, &meta); err != nil {
		t.Fatal(err)
	}
	got := meta.LocalWork.FullAuto
	if got == nil {
		t.Fatal("local_work.full_auto missing")
	}
	if len(got.Asks) != MaxReportedCommands || got.Answered != MaxReportedCommands-1 {
		t.Fatalf("asks %d answered %d, want %d asks, all but the held one answered", len(got.Asks), got.Answered, MaxReportedCommands)
	}
	if !got.Asks[0].Tainted || got.Asks[0].Held || !got.Asks[1].Held || got.Asks[1].Tainted {
		t.Fatalf("asks = %+v, want the taint and hold flags kept in order", got.Asks[:2])
	}
	if n := len(got.Asks[2].Command); n > MaxReportedCommandBytes {
		t.Fatalf("command kept %d bytes, want at most %d", n, MaxReportedCommandBytes)
	}

	encoded, err := json.Marshal(boundReport(hostReportCommit().Report))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"full_auto"`) {
		t.Fatalf("a report without Full auto must not carry it: %s", encoded)
	}
}

// TestCommitRefusesAnUnboundedFullAuto: a Full auto record with more asks
// than any commit may carry, or a count out of range, is refused, as an
// oversized command list is.
func TestCommitRefusesAnUnboundedFullAuto(t *testing.T) {
	asks := make([]FullAutoAsk, 10*MaxReportedCommands+1)
	for i := range asks {
		asks[i] = FullAutoAsk{Command: "run_command: true"}
	}
	for name, report := range map[string]*FullAutoReport{
		"too many asks":    {Answered: 1, Asks: asks},
		"negative count":   {Answered: -1, Asks: []FullAutoAsk{}},
		"count past bound": {Answered: MaxFullAutoAnswered + 1, Asks: []FullAutoAsk{}},
	} {
		store := &commitFakeStore{}
		service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
			func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		request := hostReportCommit()
		request.Report.FullAuto = report
		if _, err := service.Commit(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}
