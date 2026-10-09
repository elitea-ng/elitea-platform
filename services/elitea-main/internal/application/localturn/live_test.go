package localturn

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBindingStore struct {
	binding StoredBinding
	err     error
	reads   int
}

func (f *fakeBindingStore) ReadLocalTurnBinding(context.Context, int64, int64, string) (StoredBinding, error) {
	f.reads++
	return f.binding, f.err
}

func TestLiveTurnsAppliesTheStartAndCommitChecks(t *testing.T) {
	id := strings.Repeat("c", 32)
	credential := Credential{TokenID: "70", NativeClientID: "ai.elitea.desktop"}
	for name, test := range map[string]struct {
		policy commitFakePolicy
		store  *fakeBindingStore
		id     string
		want   error
	}{
		"live": {commitFakePolicy{allowed: true}, &fakeBindingStore{binding: StoredBinding{ApplicationID: 4, VersionID: 5, Credential: credential}}, id, nil},
		"another device": {commitFakePolicy{allowed: true}, &fakeBindingStore{binding: StoredBinding{ApplicationID: 4, VersionID: 5,
			Credential: Credential{TokenID: "71", NativeClientID: "ai.elitea.desktop"}}}, id, ErrNotFound},
		"a PAT of the same user": {commitFakePolicy{allowed: true}, &fakeBindingStore{binding: StoredBinding{ApplicationID: 4, VersionID: 5,
			Credential: Credential{TokenID: "70"}}}, id, ErrNotFound},
		"disabled":   {commitFakePolicy{allowed: false}, &fakeBindingStore{}, id, ErrLocalWorkDisabled},
		"unreadable": {commitFakePolicy{allowed: true, err: errors.New("down")}, &fakeBindingStore{}, id, ErrUnavailable},
		"not found":  {commitFakePolicy{allowed: true}, &fakeBindingStore{err: ErrNotFound}, id, ErrNotFound},
		"committed":  {commitFakePolicy{allowed: true}, &fakeBindingStore{binding: StoredBinding{Committed: true, Credential: credential}}, id, ErrAlreadyCommitted},
		"expired":    {commitFakePolicy{allowed: true}, &fakeBindingStore{binding: StoredBinding{Expired: true, Credential: credential}}, id, ErrExpired},
		"bad id":     {commitFakePolicy{allowed: true}, &fakeBindingStore{}, "not-an-id", ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			live, err := NewLiveTurns(test.store, test.policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := live.Live(context.Background(), 1, 2, credential, test.id)
			if !errors.Is(err, test.want) {
				t.Fatalf("Live = %v, want %v", err, test.want)
			}
			if test.want == nil && (turn.ApplicationID != 4 || turn.VersionID != 5 || turn.ExecutionID != id) {
				t.Fatalf("turn = %+v", turn)
			}
		})
	}
}
