package secrets

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestEnsureAdminHiddenSecretKeepsTheFirstValue: a fresh install has no
// global vault. The first call creates it and stores the generated value;
// every later call, including concurrent ones from other replicas, answers
// that same value and never calls generate again.
func TestEnsureAdminHiddenSecretKeepsTheFirstValue(t *testing.T) {
	pool := newSecretsPool(t)
	ctx := context.Background()
	handler := NewHandler(pool)

	var generated atomic.Int32
	generate := func() (string, error) {
		n := generated.Add(1)
		return "marker-" + string(rune('a'+n-1)), nil
	}
	const name = "web_push_vapid_private_key"
	values := make([]string, 8)
	var wg sync.WaitGroup
	for i := range values {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, err := handler.EnsureAdminHiddenSecret(ctx, name, generate)
			if err != nil {
				t.Errorf("ensure %d: %v", i, err)
			}
			values[i] = value
		}(i)
	}
	wg.Wait()
	for _, value := range values {
		if value == "" || value != values[0] {
			t.Fatalf("replicas disagree on the key: %v", values)
		}
	}
	stored, err := handler.LookupAdminHiddenSecret(ctx, name)
	if err != nil || stored != values[0] {
		t.Fatalf("stored = %q, %v; want %q", stored, err, values[0])
	}
	// The regular bucket does not hold it: projects never resolve it.
	vault, err := handler.adminVault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, regular := vault.Secrets[name]; regular {
		t.Fatal("the key landed in the regular bucket, which every project can interpolate")
	}

	again, err := handler.EnsureAdminHiddenSecret(ctx, name, func() (string, error) {
		return "", errors.New("generate must not run again")
	})
	if err != nil || again != values[0] {
		t.Fatalf("second ensure = %q, %v", again, err)
	}
	if _, err := handler.EnsureAdminHiddenSecret(ctx, "bad name!", generate); err == nil {
		t.Fatal("an invalid name is accepted")
	}
}
