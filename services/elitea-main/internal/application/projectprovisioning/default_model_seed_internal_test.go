package projectprovisioning

// The platform default model seed (#6826), at the level that needs no
// database: the project_secrets step asks for the seed after the vault exists,
// and a seed failure does not fail the step.

import (
	"context"
	"errors"
	"testing"
)

type recordingSeeder struct {
	seeded []int64
	err    error
}

func (s *recordingSeeder) SeedProjectModelDefault(_ context.Context, projectID int64) error {
	s.seeded = append(s.seeded, projectID)
	return s.err
}

func TestCreateProjectSecretsSeedsThePlatformDefaultModel(t *testing.T) {
	t.Parallel()

	vault := &recordingVault{}
	seeder := &recordingSeeder{}
	provisioner := New(nil, nil, nil, WithProjectVault(vault), WithDefaultModelSeeder(seeder))

	if err := createProjectSecrets(context.Background(), provisioner, &provisionState{projectID: 7}); err != nil {
		t.Fatalf("createProjectSecrets: %v", err)
	}
	if len(seeder.seeded) != 1 || seeder.seeded[0] != 7 {
		t.Fatalf("seed calls = %v, want one for project 7", seeder.seeded)
	}
	if len(vault.ensured) != 1 {
		t.Fatalf("the seed ran without the vault: ensured=%v", vault.ensured)
	}
}

// TestCreateProjectSecretsKeepsTheProjectWhenTheSeedFails pins the choice that
// the seed is a convenience: the project reads the platform default at request
// time, so failing a whole tenant over the copy would be the worse outcome.
func TestCreateProjectSecretsKeepsTheProjectWhenTheSeedFails(t *testing.T) {
	t.Parallel()

	seeder := &recordingSeeder{err: errors.New("vault write refused")}
	provisioner := New(nil, nil, nil, WithProjectVault(&recordingVault{}), WithDefaultModelSeeder(seeder))

	if err := createProjectSecrets(context.Background(), provisioner, &provisionState{projectID: 9}); err != nil {
		t.Fatalf("a seed failure failed the step: %v", err)
	}
	if len(seeder.seeded) != 1 {
		t.Fatalf("seed calls = %v, want one", seeder.seeded)
	}
}

func TestWithDefaultModelSeederTreatsATypedNilAsAbsent(t *testing.T) {
	t.Parallel()

	var typedNil *recordingSeeder
	provisioner := New(nil, nil, nil, WithProjectVault(&recordingVault{}), WithDefaultModelSeeder(typedNil))
	if provisioner.defaultModels != nil {
		t.Fatal("a typed nil seeder was stored; the step would call a nil receiver")
	}
	if err := createProjectSecrets(context.Background(), provisioner, &provisionState{projectID: 3}); err != nil {
		t.Fatalf("createProjectSecrets without a seeder: %v", err)
	}
}
