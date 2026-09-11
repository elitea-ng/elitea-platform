package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/stretchr/testify/require"
)

func TestCurrentVaultUnsecreterPortsRecursiveEmbeddedPrecedence(t *testing.T) {
	t.Parallel()

	loader := &fakeSecretVaultLoader{
		projects: map[int64]SecretVault{
			7: &fakeSecretVault{
				regular: map[string]string{
					"same":        "project-regular",
					"empty":       "",
					"replacement": "{{secret.admin_only}}",
				},
				hidden: map[string]string{
					"same":        "project-hidden-loses",
					"hidden_only": "project-hidden",
				},
			},
		},
		admin: &fakeSecretVault{
			regular: map[string]string{
				"same":       "admin-loses",
				"admin_only": "admin-regular",
			},
			hidden: map[string]string{"admin_hidden": "must-not-escape"},
		},
		projectLoads: map[int64]int{},
	}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)
	input := map[string]any{
		"embedded": "A={{secret.same}} B={{secret.hidden_only}} C={{secret.admin_only}} M={{secret.missing}}",
		"nested": map[string]any{
			"array": []any{
				"{{secret.empty}}",
				map[string]any{"value": "{{secret.replacement}}"},
				json.Number("9007199254740993"),
				true,
			},
		},
		"admin_hidden": "{{secret.admin_hidden}}",
		"invalid_name": "{{secret.not-allowed}}",
	}

	result, err := unsecreter.Unsecret(context.Background(), 7, input)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"embedded": "A=project-regular B=project-hidden C=admin-regular M={{secret.missing}}",
		"nested": map[string]any{
			"array": []any{
				"",
				map[string]any{"value": "{{secret.admin_only}}"},
				json.Number("9007199254740993"),
				true,
			},
		},
		"admin_hidden": "{{secret.admin_hidden}}",
		"invalid_name": "{{secret.not-allowed}}",
	}, result)
	require.Equal(t, 1, loader.projectLoads[7])
	require.Equal(t, 1, loader.adminLoads)

	result["nested"].(map[string]any)["array"].([]any)[1].(map[string]any)["value"] = "changed"
	require.Equal(t, "{{secret.replacement}}", input["nested"].(map[string]any)["array"].([]any)[1].(map[string]any)["value"])
}

func TestCurrentVaultUnsecreterLoadsAdminOnlyAfterProjectMiss(t *testing.T) {
	t.Parallel()

	loader := &fakeSecretVaultLoader{
		projects: map[int64]SecretVault{
			9: &fakeSecretVault{regular: map[string]string{"one": "first", "two": "second"}},
		},
		projectLoads: map[int64]int{},
	}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)

	result, err := unsecreter.Unsecret(context.Background(), 9, map[string]any{
		"value": "{{secret.one}}/{{secret.two}}/{{secret.one}}",
	})
	require.NoError(t, err)
	require.Equal(t, "first/second/first", result["value"])
	require.Equal(t, 1, loader.projectLoads[9])
	require.Zero(t, loader.adminLoads)
}

func TestCurrentVaultUnsecreterCachesAdminFallbackAndUnresolvedNames(t *testing.T) {
	t.Parallel()

	projectLookups := 0
	adminLookups := 0
	project := currentUnsecretVaultStub{
		lookup: func(string) (centrysecrets.Secret, error) {
			projectLookups++
			return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
		},
	}
	admin := currentUnsecretVaultStub{
		regular: func(string) (centrysecrets.Secret, error) {
			adminLookups++
			return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
		},
	}
	adminLoads := 0
	loader := currentUnsecretLoaderStub{
		project: func(context.Context, int64) (SecretVault, error) { return project, nil },
		admin: func(context.Context) (SecretVault, error) {
			adminLoads++
			return admin, nil
		},
	}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)

	result, err := unsecreter.Unsecret(context.Background(), 3, map[string]any{
		"one": "{{secret.missing}} {{secret.missing}}",
		"two": []any{"{{secret.missing}}"},
	})
	require.NoError(t, err)
	require.Equal(t, "{{secret.missing}} {{secret.missing}}", result["one"])
	require.Equal(t, []any{"{{secret.missing}}"}, result["two"])
	require.Equal(t, 1, projectLookups)
	require.Equal(t, 1, adminLookups)
	require.Equal(t, 1, adminLoads)
}

func TestCurrentVaultUnsecreterPreservesCancellationAndRedactsDependencies(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	loaderCalls := 0
	unsecreter, err := NewCurrentVaultUnsecreter(currentUnsecretLoaderStub{
		project: func(context.Context, int64) (SecretVault, error) {
			loaderCalls++
			return nil, nil
		},
	})
	require.NoError(t, err)
	_, err = unsecreter.Unsecret(canceled, 1, map[string]any{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, loaderCalls)

	dependencyTests := []struct {
		name   string
		loader SecretVaultLoader
		input  map[string]any
		want   error
	}{
		{
			name: "project load",
			loader: currentUnsecretLoaderStub{
				project: func(context.Context, int64) (SecretVault, error) {
					return nil, errors.New("project-vault-canary")
				},
			},
			input: map[string]any{"value": "{{secret.name}}"},
			want:  ErrCurrentUnsecretUnavailable,
		},
		{
			name: "project lookup",
			loader: currentUnsecretLoaderStub{
				project: func(context.Context, int64) (SecretVault, error) {
					return currentUnsecretVaultStub{lookup: func(string) (centrysecrets.Secret, error) {
						return centrysecrets.Secret{}, errors.New("project-lookup-canary")
					}}, nil
				},
			},
			input: map[string]any{"value": "{{secret.name}}"},
			want:  ErrCurrentUnsecretUnavailable,
		},
		{
			name: "admin load",
			loader: currentUnsecretLoaderStub{
				project: func(context.Context, int64) (SecretVault, error) {
					return currentUnsecretVaultStub{}, nil
				},
				admin: func(context.Context) (SecretVault, error) {
					return nil, errors.New("admin-vault-canary")
				},
			},
			input: map[string]any{"value": "{{secret.name}}"},
			want:  ErrCurrentUnsecretUnavailable,
		},
		{
			name: "dependency cancellation",
			loader: currentUnsecretLoaderStub{
				project: func(context.Context, int64) (SecretVault, error) {
					return nil, context.DeadlineExceeded
				},
			},
			input: map[string]any{"value": "{{secret.name}}"},
			want:  context.DeadlineExceeded,
		},
	}
	for _, test := range dependencyTests {
		t.Run(test.name, func(t *testing.T) {
			adapter, constructErr := NewCurrentVaultUnsecreter(test.loader)
			require.NoError(t, constructErr)
			_, gotErr := adapter.Unsecret(context.Background(), 1, test.input)
			require.ErrorIs(t, gotErr, test.want)
			for _, canary := range []string{"project-vault-canary", "project-lookup-canary", "admin-vault-canary"} {
				require.NotContains(t, gotErr.Error(), canary)
			}
		})
	}
}

func TestCurrentVaultUnsecreterDoesNotOpenVaultWithoutSecretReferences(t *testing.T) {
	t.Parallel()

	loaderCalls := 0
	unsecreter, err := NewCurrentVaultUnsecreter(currentUnsecretLoaderStub{
		project: func(context.Context, int64) (SecretVault, error) {
			loaderCalls++
			return nil, errors.New("must not load project vault")
		},
		admin: func(context.Context) (SecretVault, error) {
			loaderCalls++
			return nil, errors.New("must not load admin vault")
		},
	})
	require.NoError(t, err)

	input := map[string]any{
		"clear_text": "value",
		"nested":     []any{map[string]any{"enabled": true}},
	}
	result, err := unsecreter.Unsecret(context.Background(), 2, input)
	require.NoError(t, err)
	require.Equal(t, input, result)
	require.Zero(t, loaderCalls)
}

func TestCurrentVaultUnsecreterStopsOnCancellationBeforeAdminFallback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	adminCalls := 0
	loader := currentUnsecretLoaderStub{
		project: func(context.Context, int64) (SecretVault, error) {
			return currentUnsecretVaultStub{lookup: func(string) (centrysecrets.Secret, error) {
				cancel()
				return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
			}}, nil
		},
		admin: func(context.Context) (SecretVault, error) {
			adminCalls++
			return currentUnsecretVaultStub{}, nil
		},
	}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)
	_, err = unsecreter.Unsecret(ctx, 1, map[string]any{"value": "{{secret.name}}"})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, adminCalls)
}

func TestCurrentVaultUnsecreterEnforcesReferenceOutputAndSecretBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
		value  string
	}{
		{
			name:   "reference count",
			secret: "x",
			value:  strings.Repeat("{{secret.a}}", maxCurrentUnsecretReferences+1),
		},
		{
			name:   "secret size",
			secret: strings.Repeat("s", maxCurrentUnsecretValueBytes+1),
			value:  "{{secret.a}}",
		},
		{
			name:   "expanded output size",
			secret: strings.Repeat("x", 1024),
			value:  strings.Repeat("{{secret.a}}", maxCurrentUnsecretOutput/1024+1),
		},
		{
			name:   "secret name size",
			secret: "x",
			value:  "{{secret." + strings.Repeat("a", maxCurrentUnsecretNameBytes+1) + "}}",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loader := &fakeSecretVaultLoader{
				projects: map[int64]SecretVault{1: &fakeSecretVault{regular: map[string]string{"a": test.secret}}},
				admin:    &fakeSecretVault{},
			}
			unsecreter, err := NewCurrentVaultUnsecreter(loader)
			require.NoError(t, err)
			_, err = unsecreter.Unsecret(context.Background(), 1, map[string]any{"value": test.value})
			require.ErrorIs(t, err, ErrCurrentUnsecretRejected)
			require.Equal(t, ErrCurrentUnsecretRejected.Error(), err.Error())
		})
	}

	loader := &fakeSecretVaultLoader{projects: map[int64]SecretVault{1: &fakeSecretVault{}}, admin: &fakeSecretVault{}}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)
	_, err = unsecreter.Unsecret(context.Background(), 1, map[string]any{
		"value": strings.Repeat("<", maxCurrentUnsecretOutput/6+1),
	})
	require.ErrorIs(t, err, ErrCurrentUnsecretRejected)
}

func TestCurrentVaultUnsecreterRejectsInvalidConstructionAndInput(t *testing.T) {
	t.Parallel()

	_, err := NewCurrentVaultUnsecreter(nil)
	require.Error(t, err)
	loader := &fakeSecretVaultLoader{projects: map[int64]SecretVault{1: &fakeSecretVault{}}}
	unsecreter, err := NewCurrentVaultUnsecreter(loader)
	require.NoError(t, err)
	var nilContext context.Context
	_, err = unsecreter.Unsecret(nilContext, 1, map[string]any{})
	require.ErrorIs(t, err, ErrCurrentUnsecretRejected)
	_, err = unsecreter.Unsecret(context.Background(), 0, map[string]any{})
	require.ErrorIs(t, err, ErrCurrentUnsecretRejected)
}

func TestCurrentVaultUnsecreterOwnsConcurrentRequestState(t *testing.T) {
	t.Parallel()

	project := currentUnsecretVaultStub{lookup: func(name string) (centrysecrets.Secret, error) {
		if name == "token" {
			return centrysecrets.Secret{Value: "resolved"}, nil
		}
		return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
	}}
	unsecreter, err := NewCurrentVaultUnsecreter(currentUnsecretLoaderStub{
		project: func(context.Context, int64) (SecretVault, error) { return project, nil },
	})
	require.NoError(t, err)
	input := map[string]any{"nested": []any{"{{secret.token}}"}}

	const requests = 32
	errorsByRequest := make(chan string, requests)
	var group sync.WaitGroup
	group.Add(requests)
	for range requests {
		go func() {
			defer group.Done()
			result, resolveErr := unsecreter.Unsecret(context.Background(), 7, input)
			if resolveErr != nil {
				errorsByRequest <- resolveErr.Error()
				return
			}
			if got := result["nested"].([]any)[0]; got != "resolved" {
				errorsByRequest <- "unexpected result"
			}
		}()
	}
	group.Wait()
	close(errorsByRequest)
	for requestError := range errorsByRequest {
		t.Error(requestError)
	}
	require.Equal(t, "{{secret.token}}", input["nested"].([]any)[0])
}

type currentUnsecretLoaderStub struct {
	project func(context.Context, int64) (SecretVault, error)
	admin   func(context.Context) (SecretVault, error)
}

func (l currentUnsecretLoaderStub) LoadProjectVault(ctx context.Context, projectID int64) (SecretVault, error) {
	if l.project == nil {
		return nil, ErrContentUnavailable
	}
	return l.project(ctx, projectID)
}

func (l currentUnsecretLoaderStub) LoadAdminVault(ctx context.Context) (SecretVault, error) {
	if l.admin == nil {
		return nil, ErrContentUnavailable
	}
	return l.admin(ctx)
}

type currentUnsecretVaultStub struct {
	lookup  func(string) (centrysecrets.Secret, error)
	regular func(string) (centrysecrets.Secret, error)
}

func (v currentUnsecretVaultStub) Lookup(name string) (centrysecrets.Secret, error) {
	if v.lookup == nil {
		return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
	}
	return v.lookup(name)
}

func (v currentUnsecretVaultStub) LookupRegular(name string) (centrysecrets.Secret, error) {
	if v.regular == nil {
		return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
	}
	return v.regular(name)
}

func (v currentUnsecretVaultStub) LookupProjectID(name string) (centrysecrets.Secret, error) {
	return v.Lookup(name)
}

func (v currentUnsecretVaultStub) LookupRegularProjectID(name string) (centrysecrets.Secret, error) {
	return v.LookupRegular(name)
}

func (v currentUnsecretVaultStub) LookupRegularInteger(name string) (centrysecrets.Secret, error) {
	return v.LookupRegular(name)
}
