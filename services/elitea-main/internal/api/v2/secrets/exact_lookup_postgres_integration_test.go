package secrets

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestExactLookupPreservesUnrelatedTypedVaultValues(t *testing.T) {
	pool := newSecretsPool(t)
	seedProjectVault(t, pool, "7")
	h := NewHandler(pool)
	keyRow, _ := rawVaultBlobs(t, pool, dbKey("7"))
	key, err := h.decryptKey(keyRow)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	encrypted, err := fernetEncrypt(key, []byte(`{"secrets":{"context_manager":"false"},"hidden_secrets":{"context_manager":"true","model_project_id":2,"feature":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE centry.secrets_data SET data=$1 WHERE id=$2`, encrypted, dbKey("7")); err != nil {
		t.Fatal(err)
	}
	value, err := h.ResolveSecretValue(context.Background(), "7", "{{secret.context_manager}}")
	if err != nil || value != "false" {
		t.Fatalf("exact regular lookup: %q %v", value, err)
	}
	if _, err = h.ResolveSecretValue(context.Background(), "7", "{{secret.missing}}"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err = h.ResolveSecretValue(context.Background(), "7", "{{secret.model_project_id}}"); err == nil {
		t.Fatal("numeric credential accepted as string")
	}
	afterKey, afterData := rawVaultBlobs(t, pool, dbKey("7"))
	if !bytes.Equal(keyRow, afterKey) || !bytes.Equal(encrypted, afterData) {
		t.Fatal("exact lookup rewrites the vault")
	}
}
