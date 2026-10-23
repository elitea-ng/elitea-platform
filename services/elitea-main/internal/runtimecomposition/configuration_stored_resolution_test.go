package runtimecomposition

// The stored-configuration resolver (configuration_stored_resolution.go): the
// capability that lets a SAVED credential be checked without the client
// resending its api_key.
//
// It is the sibling of currentProviderConfigurationResolution and differs in
// exactly one way — it RETURNS the plaintext instead of dropping it — so these
// cases pin the two things that must not differ: the order of the walk, and
// the refusal to hand back anything at all when a collaborator is absent.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	configurationapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentStoredConfigurationResolutionUnsecretsTheOwnerThenExpands(t *testing.T) {
	callOrder := []string{}
	unsecreter := currentLifecycleUnsecreterStub{unsecret: func(
		_ context.Context,
		projectID int32,
		data map[string]any,
	) (map[string]any, error) {
		callOrder = append(callOrder, "unsecret")
		if projectID != 7 || data["api_key"] != "{{secret.key}}" {
			t.Fatalf("project=%d data=%#v", projectID, data)
		}
		return map[string]any{"api_key": "redeemed", "api_base": "https://api.openai.com/v1"}, nil
	}}
	expander := currentLifecycleExpanderStub{expand: func(
		_ context.Context,
		request configurationapp.CurrentExpansionRequest,
	) (map[string]any, error) {
		callOrder = append(callOrder, "expand")
		// Unsecret must stay true so a NESTED configuration's own secrets are
		// redeemed through that configuration's project vault, and the author
		// must travel with the request or a `private: true` reference cannot
		// resolve.
		if request.CurrentProjectID != 7 || !request.Unsecret ||
			request.UserID == nil || *request.UserID != 13 ||
			request.Payload["api_key"] != "redeemed" {
			t.Fatalf("request=%#v", request)
		}
		return map[string]any{"api_key": "redeemed", "api_base": "https://api.openai.com/v1"}, nil
	}}

	resolution := &CurrentStoredConfigurationResolution{expander: expander, unsecreter: unsecreter}
	author := int32(13)
	stored := map[string]any{"api_key": "{{secret.key}}", "api_base": "https://api.openai.com/v1"}

	resolved, err := resolution.ResolveStoredConfiguration(
		context.Background(),
		configurationapi.StoredConfigurationResolution{ProjectID: 7, AuthorID: &author, Data: stored},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(callOrder, []string{"unsecret", "expand"}) {
		t.Fatalf("calls=%#v; the owner's own references must be redeemed before expansion", callOrder)
	}
	if resolved["api_key"] != "redeemed" {
		t.Fatalf("resolved=%#v; the caller must receive the redeemed value, not the reference", resolved)
	}
	if stored["api_key"] != "{{secret.key}}" {
		t.Fatalf("the stored row was mutated: %#v", stored)
	}
}

// A collaborator that fails answers ONE opaque sentinel. A dependency's own
// error text can name another project's configuration, and this value travels
// to a handler that turns it into a browser message.
func TestCurrentStoredConfigurationResolutionReportsOneOpaqueFailure(t *testing.T) {
	for name, resolution := range map[string]*CurrentStoredConfigurationResolution{
		"unsecret fails": {
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(
				context.Context, int32, map[string]any,
			) (map[string]any, error) {
				return nil, errors.New("vault project-7 could not open")
			}},
			expander: currentLifecycleExpanderStub{expand: func(
				context.Context, configurationapp.CurrentExpansionRequest,
			) (map[string]any, error) {
				t.Fatal("expansion ran after a failed unsecret")
				return nil, nil
			}},
		},
		"expansion fails": {
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(
				_ context.Context, _ int32, data map[string]any,
			) (map[string]any, error) {
				return data, nil
			}},
			expander: currentLifecycleExpanderStub{expand: func(
				context.Context, configurationapp.CurrentExpansionRequest,
			) (map[string]any, error) {
				return nil, errors.New("configuration \"other project's credential\" not found")
			}},
		},
		"expansion returns nothing": {
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(
				_ context.Context, _ int32, data map[string]any,
			) (map[string]any, error) {
				return data, nil
			}},
			expander: currentLifecycleExpanderStub{expand: func(
				context.Context, configurationapp.CurrentExpansionRequest,
			) (map[string]any, error) {
				return nil, nil
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := resolution.ResolveStoredConfiguration(
				context.Background(),
				configurationapi.StoredConfigurationResolution{
					ProjectID: 7, Data: map[string]any{"api_key": "{{secret.key}}"},
				},
			)
			if resolved != nil {
				t.Fatalf("a failed resolution returned a payload: %#v", resolved)
			}
			if !errors.Is(err, errCurrentStoredConfigurationResolution) {
				t.Fatalf("err = %v, want the opaque sentinel", err)
			}
		})
	}
}

// A cancelled context is returned as ITSELF, so the caller can tell "no answer
// was reached" from "this row does not resolve". The first must not be shown
// to a user as a broken credential.
func TestCurrentStoredConfigurationResolutionReturnsContextFailuresUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolution := &CurrentStoredConfigurationResolution{
		unsecreter: currentLifecycleUnsecreterStub{unsecret: func(
			context.Context, int32, map[string]any,
		) (map[string]any, error) {
			t.Fatal("the walk started on a cancelled context")
			return nil, nil
		}},
		expander: currentLifecycleExpanderStub{expand: func(
			context.Context, configurationapp.CurrentExpansionRequest,
		) (map[string]any, error) {
			return nil, nil
		}},
	}

	if _, err := resolution.ResolveStoredConfiguration(
		ctx,
		configurationapi.StoredConfigurationResolution{ProjectID: 7, Data: map[string]any{}},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// THE TYPED-NIL CASE. A composition root that assigns the constructor's result
// unconditionally boxes a nil POINTER into the interface, and every nil test
// downstream is then false. The method must answer rather than dereference —
// the handler cannot defend against this, because from its side the interface
// is not nil.
func TestATypedNilStoredResolverAnswersInsteadOfPanicking(t *testing.T) {
	var resolution *CurrentStoredConfigurationResolution
	var resolver configurationapi.StoredConfigurationResolver = resolution
	// `resolver == nil` is deliberately NOT asserted here. staticcheck proves
	// that comparison is never true (SA4023), which IS the property under
	// test: the boxed nil pointer is not a nil interface, so no nil test at
	// any call site downstream can catch it. The guard has to be in the
	// method, and that is what the call below measures.

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("a typed-nil resolver panicked with %v", recovered)
		}
	}()
	resolved, err := resolver.ResolveStoredConfiguration(
		context.Background(),
		configurationapi.StoredConfigurationResolution{ProjectID: 7, Data: map[string]any{}},
	)
	if resolved != nil || err == nil {
		t.Fatalf("resolved=%#v err=%v, want no payload and an error", resolved, err)
	}
}

// The composition itself refuses rather than degrade: a resolver with no vault
// could only hand back the stored {{secret.NAME}} reference, and the check
// would report every working credential as rejected.
func TestNewCurrentStoredConfigurationResolverRefusesAnIncompleteRuntime(t *testing.T) {
	for name, runtime := range map[string]*CurrentConfigurationsRuntime{
		"no runtime":    nil,
		"no expander":   {unsecreter: nil},
		"no unsecreter": {},
	} {
		t.Run(name, func(t *testing.T) {
			resolver, err := NewCurrentStoredConfigurationResolver(runtime)
			if err == nil {
				t.Fatal("an incomplete runtime composed a resolver")
			}
			if resolver != nil {
				t.Fatalf("a refused composition returned %#v", resolver)
			}
		})
	}
}
