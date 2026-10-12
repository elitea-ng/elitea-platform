package projectprovisioning

// The project_secrets step, at the level that needs no database (#408).
//
// The integration cases beside this file prove the value survives a real
// vault. These prove the STEP asks for one at all, which is the half a
// database cannot show: a step that quietly skipped the call would still
// create a readable vault, and the model picker — the only thing the vault
// step used to be checked against — would still work.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type recordingVault struct {
	ensured       []string
	headerValued  []string
	removed       []string
	alreadyHeld   bool
	ensureErr     error
	headerValErr  error
	removeVaultOK bool
}

func (v *recordingVault) EnsureProjectVault(_ context.Context, projectID string) error {
	v.ensured = append(v.ensured, projectID)
	return v.ensureErr
}

func (v *recordingVault) EnsureProjectSecretsHeaderValue(
	_ context.Context, projectID string,
) (bool, error) {
	v.headerValued = append(v.headerValued, projectID)
	if v.headerValErr != nil {
		return false, v.headerValErr
	}
	return !v.alreadyHeld, nil
}

func (v *recordingVault) RemoveProjectVault(_ context.Context, projectID string) error {
	v.removed = append(v.removed, projectID)
	v.removeVaultOK = true
	return nil
}

func (v *recordingVault) RemoveProjectVaultTx(ctx context.Context, _ pgx.Tx, projectID string) error {
	return v.RemoveProjectVault(ctx, projectID)
}

func TestCreateProjectSecretsSealsTheHeaderValue(t *testing.T) {
	t.Parallel()

	vault := &recordingVault{}
	provisioner := New(nil, nil, nil, WithProjectVault(vault))
	state := &provisionState{projectID: 7}

	if err := createProjectSecrets(context.Background(), provisioner, state); err != nil {
		t.Fatalf("createProjectSecrets: %v", err)
	}
	if len(vault.ensured) != 1 || vault.ensured[0] != "7" {
		t.Fatalf("EnsureProjectVault calls = %v, want one for project 7", vault.ensured)
	}
	if len(vault.headerValued) != 1 || vault.headerValued[0] != "7" {
		t.Fatalf("EnsureProjectSecretsHeaderValue calls = %v; without one the project "+
			"accepts the literal \"secret\"", vault.headerValued)
	}
}

// TestCreateProjectSecretsFailsWhenTheHeaderValueCannotBeSealed keeps the step
// honest. A project reported as provisioned while its value was never written
// is the exact state #408 describes, and reporting success would hide it.
func TestCreateProjectSecretsFailsWhenTheHeaderValueCannotBeSealed(t *testing.T) {
	t.Parallel()

	sealFailed := errors.New("vault write refused")
	vault := &recordingVault{headerValErr: sealFailed}
	provisioner := New(nil, nil, nil, WithProjectVault(vault))

	err := createProjectSecrets(context.Background(), provisioner, &provisionState{projectID: 7})

	if err == nil {
		t.Fatal("the step reported success while the X-SECRET value was not written")
	}
	if !errors.Is(err, sealFailed) {
		t.Fatalf("error = %v, want it to carry the cause for the step log", err)
	}
}

// TestCreateProjectSecretsKeepsAValueThatIsAlreadyThere pins the convergence
// rule at the step level. The value is a shared credential: a re-run that
// replaced it would refuse the SDK calls already carrying the old one.
func TestCreateProjectSecretsKeepsAValueThatIsAlreadyThere(t *testing.T) {
	t.Parallel()

	vault := &recordingVault{alreadyHeld: true}
	provisioner := New(nil, nil, nil, WithProjectVault(vault))

	if err := createProjectSecrets(context.Background(), provisioner, &provisionState{projectID: 7}); err != nil {
		t.Fatalf("createProjectSecrets over an existing value: %v", err)
	}
}

// TestRemoveProjectSecretsCompensatesTheHeaderValue proves the compensation
// half. The value lives in the vault rows, so the vault delete is what removes
// it; a step that created a value and compensated nothing would leave the
// successor project holding a credential that was already handed out.
func TestRemoveProjectSecretsCompensatesTheHeaderValue(t *testing.T) {
	t.Parallel()

	vault := &recordingVault{}
	provisioner := New(nil, nil, nil, WithProjectVault(vault))
	state := &provisionState{projectID: 7}

	if err := createProjectSecrets(context.Background(), provisioner, state); err != nil {
		t.Fatalf("createProjectSecrets: %v", err)
	}
	if err := removeProjectSecrets(context.Background(), provisioner, state); err != nil {
		t.Fatalf("removeProjectSecrets: %v", err)
	}
	if len(vault.removed) != 1 || vault.removed[0] != "7" {
		t.Fatalf("RemoveProjectVault calls = %v, want one for project 7", vault.removed)
	}
}

// TestProjectSecretsStepIsInTheCompensationList reads the pipeline itself. A
// create half with no remove half compensates nothing, and the list is the
// only place that pairing is stated.
func TestProjectSecretsStepIsInTheCompensationList(t *testing.T) {
	t.Parallel()

	for _, step := range createSteps() {
		if step.name != StepProjectSecrets {
			continue
		}
		if step.create == nil || step.remove == nil {
			t.Fatalf("the %s step has create=%v remove=%v", StepProjectSecrets, step.create != nil, step.remove != nil)
		}
		return
	}
	t.Fatalf("the pipeline holds no %s step", StepProjectSecrets)
}
