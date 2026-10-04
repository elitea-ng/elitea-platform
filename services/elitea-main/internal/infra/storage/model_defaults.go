package storage

import (
	"context"
	"errors"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

var ErrCurrentModelDefaultsUnavailable = errors.New("current model defaults are unavailable")

// CurrentModelDefaultsReader reproduces the current VaultClient lookup path:
// project regular secrets first, then public get_all_secrets precedence
// (public regular, public hidden, admin regular) for each missing field.
type CurrentModelDefaultsReader struct {
	vaults SecretVaultLoader
}

func NewCurrentModelDefaultsReader(vaults SecretVaultLoader) (*CurrentModelDefaultsReader, error) {
	if vaults == nil {
		return nil, errors.New("current model secret vault loader is required")
	}
	return &CurrentModelDefaultsReader{vaults: vaults}, nil
}

func (r *CurrentModelDefaultsReader) Load(
	ctx context.Context,
	projectID, publicProjectID int32,
	section configurationapp.CurrentModelSection,
) (configurationapp.CurrentModelCatalogDefaults, error) {
	if ctx == nil || projectID <= 0 || publicProjectID <= 0 || !configurationapp.IsSupportedCurrentModelSection(section) {
		return configurationapp.CurrentModelCatalogDefaults{}, configurationapp.ErrInvalidCurrentModelCatalogRequest
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentModelCatalogDefaults{}, err
	}

	// A project that has stored no secret has no vault, and a deployment where
	// nobody has written an admin secret has no admin vault. That is the state
	// of every fresh install. It means "no default has been chosen", which is
	// exactly what an empty CurrentModelCatalogDefaults says.
	//
	// Refusing it instead made GET /api/v2/configurations/models/{projectID}
	// answer 500 for EVERY section, so the model picker was empty on a
	// deployment whose model rows were all present and readable. The catalogue
	// is the models; the defaults only mark one of them.
	projectVault, err := r.vaults.LoadProjectVault(ctx, int64(projectID))
	if err != nil && !errors.Is(err, ErrVaultAbsent) {
		return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
	}
	if errors.Is(err, ErrVaultAbsent) {
		projectVault = nil
	}
	if err == nil && projectVault == nil {
		return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, nil)
	}

	var defaults configurationapp.CurrentModelCatalogDefaults
	bindings := currentModelDefaultBindings(section, &defaults)
	needsPublicFallback := projectVault == nil
	for _, binding := range bindings {
		if projectVault == nil {
			continue
		}
		binding.sources.Project, err = readCurrentProjectModelDefault(ctx, projectVault, binding.prefix)
		if err != nil {
			return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
		}
		if binding.sources.Project.Name == "" || binding.sources.Project.ProjectID == "" {
			needsPublicFallback = true
		}
	}
	if !needsPublicFallback {
		return defaults, nil
	}

	// EngineBase.get_all_secrets reads admin regular secrets, then overlays the
	// public project's hidden and regular collections. Load the same sources;
	// hidden admin values are deliberately outside this compatibility path.
	adminVault, err := r.loadFallbackVault(ctx, func() (SecretVault, error) {
		return r.vaults.LoadAdminVault(ctx)
	})
	if err != nil {
		return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentModelCatalogDefaults{}, err
	}
	publicVault := projectVault
	if projectID != publicProjectID {
		publicVault, err = r.loadFallbackVault(ctx, func() (SecretVault, error) {
			return r.vaults.LoadProjectVault(ctx, int64(publicProjectID))
		})
		if err != nil {
			return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
		}
	}
	if publicVault == nil {
		publicVault = emptySecretVault{}
	}

	for _, binding := range bindings {
		if err := ctx.Err(); err != nil {
			return configurationapp.CurrentModelCatalogDefaults{}, err
		}
		if binding.sources.Project.Name == "" {
			binding.sources.Public.Name, err = lookupCurrentModelNameFromAll(publicVault, adminVault, binding.prefix)
			if err != nil {
				return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
			}
		}
		if binding.sources.Project.ProjectID == "" {
			binding.sources.Public.ProjectID, err = lookupCurrentModelProjectIDFromAll(publicVault, adminVault, binding.prefix)
			if err != nil {
				return configurationapp.CurrentModelCatalogDefaults{}, currentModelDefaultsFailure(ctx, err)
			}
		}
	}
	return defaults, nil
}

// loadFallbackVault loads one of the fallback vaults, reading absence as an
// empty vault. A vault that exists and will not open is still a failure: its
// values are there and unread, so answering "no default" would be a claim this
// process cannot make.
func (r *CurrentModelDefaultsReader) loadFallbackVault(
	ctx context.Context,
	load func() (SecretVault, error),
) (SecretVault, error) {
	vault, err := load()
	if errors.Is(err, ErrVaultAbsent) {
		return emptySecretVault{}, nil
	}
	if err != nil {
		return nil, err
	}
	if vault == nil {
		return nil, ErrCurrentModelDefaultsUnavailable
	}
	return vault, nil
}

// emptySecretVault is the vault of a scope that holds no secrets. Every lookup
// answers ErrSecretNotFound, which is what each caller already handles as "this
// value was never set".
type emptySecretVault struct{}

func (emptySecretVault) Lookup(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

func (emptySecretVault) LookupRegular(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

func (emptySecretVault) LookupProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

func (emptySecretVault) LookupRegularProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

func (emptySecretVault) LookupRegularInteger(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

// The two Python-integer accessors are not part of SecretVault. They are here
// so that a caller which type-asserts for them — the chat configuration read —
// gets the same "never set" answer from an absent vault as from a vault that
// does not carry the key.
func (emptySecretVault) LookupPythonInteger(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

func (emptySecretVault) LookupRegularPythonInteger(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}

// AbsentSecretVault is the vault of a scope that holds no secrets. It is the
// value a caller substitutes when a load answered ErrVaultAbsent.
func AbsentSecretVault() SecretVault { return emptySecretVault{} }

var _ SecretVault = emptySecretVault{}

type currentModelDefaultBinding struct {
	prefix  string
	sources *configurationapp.CurrentModelDefaultSources
}

func currentModelDefaultBindings(
	section configurationapp.CurrentModelSection,
	defaults *configurationapp.CurrentModelCatalogDefaults,
) []currentModelDefaultBinding {
	bindings := []currentModelDefaultBinding{{prefix: string(section), sources: &defaults.Model}}
	if section == configurationapp.CurrentModelSectionLLM {
		bindings = append(bindings,
			currentModelDefaultBinding{prefix: "llm_low_tier", sources: &defaults.LowTier},
			currentModelDefaultBinding{prefix: "llm_high_tier", sources: &defaults.HighTier},
		)
	}
	return bindings
}

func readCurrentProjectModelDefault(
	ctx context.Context,
	vault SecretVault,
	prefix string,
) (configurationapp.CurrentModelDefault, error) {
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentModelDefault{}, err
	}
	nameKey, projectIDKey := currentModelDefaultKeys(prefix)
	name, err := vault.LookupRegular(nameKey)
	if err != nil && !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return configurationapp.CurrentModelDefault{}, err
	}
	if errors.Is(err, centrysecrets.ErrSecretNotFound) {
		name = centrysecrets.Secret{}
	}

	projectID, err := vault.LookupRegularProjectID(projectIDKey)
	if err != nil && !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return configurationapp.CurrentModelDefault{}, err
	}
	if errors.Is(err, centrysecrets.ErrSecretNotFound) {
		projectID = centrysecrets.Secret{}
	}
	return configurationapp.CurrentModelDefault{Name: name.Value, ProjectID: projectID.Value}, nil
}

func lookupCurrentModelNameFromAll(publicVault, adminVault SecretVault, prefix string) (string, error) {
	nameKey, _ := currentModelDefaultKeys(prefix)
	secret, err := publicVault.Lookup(nameKey)
	if err == nil {
		return secret.Value, nil
	}
	if !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", err
	}
	secret, err = adminVault.LookupRegular(nameKey)
	if errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return secret.Value, nil
}

func lookupCurrentModelProjectIDFromAll(publicVault, adminVault SecretVault, prefix string) (string, error) {
	_, projectIDKey := currentModelDefaultKeys(prefix)
	secret, err := publicVault.LookupProjectID(projectIDKey)
	if err == nil {
		return secret.Value, nil
	}
	if !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", err
	}
	secret, err = adminVault.LookupRegularProjectID(projectIDKey)
	if errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return secret.Value, nil
}

func currentModelDefaultKeys(prefix string) (string, string) {
	return "default_" + prefix + "_model_name", "default_" + prefix + "_model_project_id"
}

func currentModelDefaultsFailure(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrCurrentModelDefaultsUnavailable
}

// LoadPlatformModelDefault reads the platform default model (#6826): the
// public project's stored default, then the admin vault's regular value, per
// field. It is the Public half of Load with no project read in front of it.
// An absent vault is "not set".
func (r *CurrentModelDefaultsReader) LoadPlatformModelDefault(
	ctx context.Context,
	publicProjectID int32,
	section configurationapp.CurrentModelSection,
) (configurationapp.CurrentModelDefault, error) {
	if ctx == nil || publicProjectID <= 0 || !configurationapp.IsSupportedCurrentModelSection(section) {
		return configurationapp.CurrentModelDefault{}, configurationapp.ErrInvalidCurrentModelCatalogRequest
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentModelDefault{}, err
	}
	publicVault, err := r.loadFallbackVault(ctx, func() (SecretVault, error) {
		return r.vaults.LoadProjectVault(ctx, int64(publicProjectID))
	})
	if err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	adminVault, err := r.loadFallbackVault(ctx, func() (SecretVault, error) {
		return r.vaults.LoadAdminVault(ctx)
	})
	if err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	var result configurationapp.CurrentModelDefault
	if result.Name, err = lookupCurrentModelNameFromAll(publicVault, adminVault, string(section)); err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	if result.ProjectID, err = lookupCurrentModelProjectIDFromAll(publicVault, adminVault, string(section)); err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	return result, nil
}

// LoadProjectModelDefault reads one project's OWN stored default, with no
// public or admin fallback. A project with no vault answers "not set".
func (r *CurrentModelDefaultsReader) LoadProjectModelDefault(
	ctx context.Context,
	projectID int32,
	section configurationapp.CurrentModelSection,
) (configurationapp.CurrentModelDefault, error) {
	if ctx == nil || projectID <= 0 || !configurationapp.IsSupportedCurrentModelSection(section) {
		return configurationapp.CurrentModelDefault{}, configurationapp.ErrInvalidCurrentModelCatalogRequest
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentModelDefault{}, err
	}
	vault, err := r.loadFallbackVault(ctx, func() (SecretVault, error) {
		return r.vaults.LoadProjectVault(ctx, int64(projectID))
	})
	if err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	result, err := readCurrentProjectModelDefault(ctx, vault, string(section))
	if err != nil {
		return configurationapp.CurrentModelDefault{}, currentModelDefaultsFailure(ctx, err)
	}
	return result, nil
}

var _ configurationapp.PlatformModelDefaultStore = (*CurrentModelDefaultsReader)(nil)
