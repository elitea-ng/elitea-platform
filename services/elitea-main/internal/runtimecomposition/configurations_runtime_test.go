package runtimecomposition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentConfigurationsRuntimeIsIndependentOfWorkerRuntime(t *testing.T) {
	runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Reader() == nil || runtime.Types() == nil || runtime.Expansion() == nil || runtime.ModelCatalog() == nil || runtime.AvailableCatalog() == nil || runtime.VaultWriter() == nil {
		t.Fatalf("incomplete current Configurations runtime: %+v", runtime)
	}
	validator := currentConfigurationsSDKValidatorStub{}
	if service, err := runtime.NewMutationService(validator); err != nil || service == nil {
		t.Fatalf("compose mutation service=%+v error=%v", service, err)
	}
	if service, err := runtime.NewMutationService(nil); service != nil || err == nil {
		t.Fatalf("incomplete normalizer composed service=%+v error=%v", service, err)
	}
	runtime.Destroy()
	if runtime.Reader() != nil || runtime.Types() != nil || runtime.Expansion() != nil || runtime.ModelCatalog() != nil || runtime.AvailableCatalog() != nil || runtime.VaultWriter() != nil {
		t.Fatal("destroyed current Configurations runtime retained services")
	}
	if service, err := runtime.NewMutationService(validator); service != nil || err == nil {
		t.Fatalf("destroyed runtime composed service=%+v error=%v", service, err)
	}
}

type currentConfigurationsSDKValidatorStub struct{}

func (currentConfigurationsSDKValidatorStub) ValidateCurrentSDKConfiguration(
	context.Context,
	configurationapp.CurrentSDKConfigurationValidationRequest,
) error {
	return nil
}

func TestCurrentConfigurationsRuntimeRejectsMissingDatabase(t *testing.T) {
	if runtime, err := NewCurrentConfigurationsRuntime(nil, 1, "", nil); runtime != nil || err == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	if runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 0, "", nil); runtime != nil || err == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
}

// The env key must reach the vault LOADER, not just be accepted and dropped.
// A key that only the writer holds is the shape that broke: the secrets
// handler wrapped every project key with SECRETS_MASTER_KEY while this
// runtime's loader was built with no key at all, so it opened wrapped rows as
// unwrapped and answered ErrInvalidProjectKey to every model-catalogue read.
// An invalid key is the observable proof of arrival — the loader validates it
// and refuses, which a dropped key could not do.
func TestCurrentConfigurationsRuntimeGivesTheEnvironmentMasterKeyToTheVaultLoader(t *testing.T) {
	if runtime, err := NewCurrentConfigurationsRuntime(
		&pgxpool.Pool{}, 1, "", []byte("not-a-fernet-key"),
	); runtime != nil || err == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	valid := bytes.Repeat([]byte("A"), 43)
	valid = append(valid, '=')
	if runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 1, "", valid); runtime == nil || err != nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	} else {
		runtime.Destroy()
	}
}

// Two sources that disagree are refused rather than resolved. Whichever key
// lost would open some vaults in the database and not others, and the half it
// could not open is indistinguishable from a corrupt row.
func TestCurrentConfigurationsRuntimeRefusesDisagreeingMasterKeys(t *testing.T) {
	fileKey := bytes.Repeat([]byte("A"), 43)
	fileKey = append(fileKey, '=')
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, fileKey, 0o600); err != nil {
		t.Fatal(err)
	}
	envKey := bytes.Repeat([]byte("B"), 43)
	envKey = append(envKey, '=')
	if runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 1, path, envKey); runtime != nil || err == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	// The same key from both sources is not a conflict.
	agreeing := append([]byte(nil), fileKey...)
	if runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 1, path, agreeing); runtime == nil || err != nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	} else {
		runtime.Destroy()
	}
}

// Agreement is about the KEY, not its spelling. A 44-character Fernet key ends
// in one '=' pad, so its final three characters carry 2 bytes in 18 bits and 2
// bits go unused — and Go's base64 decoder accepts whatever is in them. Two
// spellings of one key must not be read as two keys and refuse to boot a
// deployment that is in fact consistent.
func TestCurrentConfigurationsRuntimeComparesMasterKeysByValueNotSpelling(t *testing.T) {
	fileKey := bytes.Repeat([]byte("A"), 43)
	fileKey = append(fileKey, '=')
	// Same 32 bytes, different text: only the discarded trailing bits differ.
	envKey := bytes.Repeat([]byte("A"), 42)
	envKey = append(envKey, 'B', '=')
	decodedFile, ok := decodeFernetMasterKey(fileKey)
	if !ok {
		t.Fatal("fixture: the file key does not decode")
	}
	decodedEnv, ok := decodeFernetMasterKey(envKey)
	if !ok {
		t.Fatal("fixture: the env key does not decode")
	}
	if !bytes.Equal(decodedFile, decodedEnv) {
		t.Fatalf("fixture: the two spellings decode differently (%x vs %x)", decodedFile, decodedEnv)
	}
	if bytes.Equal(fileKey, envKey) {
		t.Fatal("fixture: the two spellings are textually identical, so this proves nothing")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, fileKey, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCurrentConfigurationsRuntime(&pgxpool.Pool{}, 1, path, envKey)
	if runtime == nil || err != nil {
		t.Fatalf("two spellings of one key were refused: runtime=%+v error=%v", runtime, err)
	}
	runtime.Destroy()
}
