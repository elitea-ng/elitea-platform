package deepwiki_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	deepwiki "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

func env(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

// A misspelled flag must not read as "off". The whole class of defect this
// guards is an operator who believes a feature is on, a facade that is not
// mounted, and nothing anywhere saying so.
func TestAnUnparseableEnabledFlagIsAnErrorNotFalse(t *testing.T) {
	for _, spelling := range []string{"ture", "enabled", "yes please", "2", "-1"} {
		if _, err := deepwiki.ConfigFromEnv(env(map[string]string{
			deepwiki.EnabledEnv: spelling,
		})); err == nil {
			t.Fatalf("%q was accepted; a typo would silently disable the facade", spelling)
		}
	}

	for _, spelling := range []string{"1", "true", "TRUE", "yes", "on"} {
		cfg, err := deepwiki.ConfigFromEnv(env(map[string]string{
			deepwiki.EnabledEnv: spelling,
		}))
		if err != nil || !cfg.Enabled {
			t.Fatalf("%q: enabled=%v err=%v", spelling, cfg.Enabled, err)
		}
	}
}

func TestAnAbsentFlagLeavesTheFacadeOffAndNeedsNothingElse(t *testing.T) {
	cfg, err := deepwiki.ConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("absent flag enabled the facade")
	}
	// A deployment that does not run the provider must not be told it is
	// missing certificates for a service it never asked for.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled facade demanded configuration: %v", err)
	}
}

// Enabled without the mTLS material is caught at composition. The provider
// terminates mTLS with CERT_REQUIRED, so such a facade would answer every call
// with a transport failure while the flag says the feature is on.
func TestEnabledWithoutMTLSMaterialIsRefused(t *testing.T) {
	complete := map[string]string{
		deepwiki.EnabledEnv:    "true",
		deepwiki.TargetEnv:     "elitea-deepwiki:9443",
		deepwiki.ClientCertEnv: "/tls/tls.crt",
		deepwiki.ClientKeyEnv:  "/tls/tls.key",
		deepwiki.CAFileEnv:     "/tls/ca.crt",
	}

	cfg, err := deepwiki.ConfigFromEnv(env(complete))
	if err != nil || cfg.Validate() != nil {
		t.Fatalf("a complete configuration was refused: err=%v validate=%v", err, cfg.Validate())
	}

	for _, missing := range []string{
		deepwiki.TargetEnv,
		deepwiki.ClientCertEnv,
		deepwiki.ClientKeyEnv,
		deepwiki.CAFileEnv,
	} {
		partial := map[string]string{}
		for key, value := range complete {
			partial[key] = value
		}
		delete(partial, missing)

		cfg, err := deepwiki.ConfigFromEnv(env(partial))
		if err != nil {
			t.Fatal(err)
		}
		validateErr := cfg.Validate()
		if !errors.Is(validateErr, deepwiki.ErrIncompleteConfig) {
			t.Fatalf("missing %s was accepted: %v", missing, validateErr)
		}
	}
}

// ELITEA_DEEPWIKI_CALLBACK_BASE_URL is retired (#1359 U5): the engine calls
// back over the platform gRPC listener. A value an older deployment still sets
// is refused, naming the variable, whether the facade is on or off.
func TestTheRetiredCallbackOriginIsRefused(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		_, err := deepwiki.ConfigFromEnv(env(map[string]string{
			deepwiki.EnabledEnv:                enabled,
			deepwiki.RetiredCallbackBaseURLEnv: "https://elitea-main:9446",
		}))
		if err == nil || !strings.Contains(err.Error(), deepwiki.RetiredCallbackBaseURLEnv) {
			t.Fatalf("enabled=%s: the retired variable was not refused: %v", enabled, err)
		}
	}
}

func TestTheTimeoutIsParsedStrictlyAndDefaults(t *testing.T) {
	cfg, err := deepwiki.ConfigFromEnv(env(map[string]string{deepwiki.EnabledEnv: "0"}))
	if err != nil || cfg.Timeout != 30*time.Second {
		t.Fatalf("default timeout %v (err %v)", cfg.Timeout, err)
	}

	cfg, err = deepwiki.ConfigFromEnv(env(map[string]string{
		deepwiki.TimeoutEnv: "90",
	}))
	if err != nil || cfg.Timeout != 90*time.Second {
		t.Fatalf("timeout %v (err %v)", cfg.Timeout, err)
	}

	// Zero and negative are refused rather than clamped: a caller who wrote
	// them meant something, and neither meaning is "the default".
	for _, raw := range []string{"0", "-5", "thirty", "30s"} {
		if _, err := deepwiki.ConfigFromEnv(env(map[string]string{
			deepwiki.TimeoutEnv: raw,
		})); err == nil {
			t.Fatalf("timeout %q was accepted", raw)
		}
	}
}

// The retired HTTPS origin is a boot error naming its replacement (#1359):
// the provider serves no HTTP any more, and reading the old value as nothing
// would leave the operator with a facade that cannot say why.
func TestTheRetiredBaseURLIsRefused(t *testing.T) {
	_, err := deepwiki.ConfigFromEnv(env(map[string]string{
		deepwiki.EnabledEnv:        "true",
		"ELITEA_DEEPWIKI_BASE_URL": "https://elitea-deepwiki:8080",
	}))
	if !errors.Is(err, facade.ErrIncompleteConfig) || !strings.Contains(err.Error(), deepwiki.TargetEnv) {
		t.Fatalf("the retired ELITEA_DEEPWIKI_BASE_URL was not refused by name: %v", err)
	}
}

// The callback bearer outlives the longest build the worker's default CPU cap
// allows (4 h of CPU, plus the wall-clock waiting on models it does not
// count): the engine writes its vectors with it at the END of the build.
func TestTheCallbackTokenTTLCoversTheLongestBuildAndIsOverridable(t *testing.T) {
	cfg, err := deepwiki.ConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	const defaultWorkerCPU = 4 * time.Hour // ELITEA_DEEPWIKI_WORKER_CPU_SECONDS default
	if cfg.CallbackTokenTTL != deepwiki.DefaultCallbackTokenTTL || cfg.CallbackTokenTTL <= defaultWorkerCPU {
		t.Fatalf("default callback TTL %v, want %v and above the %v CPU cap",
			cfg.CallbackTokenTTL, deepwiki.DefaultCallbackTokenTTL, defaultWorkerCPU)
	}
	cfg, err = deepwiki.ConfigFromEnv(env(map[string]string{deepwiki.CallbackTokenTTLEnv: "90"}))
	if err != nil || cfg.CallbackTokenTTL != 90*time.Minute {
		t.Fatalf("override %v (err %v)", cfg.CallbackTokenTTL, err)
	}
	for _, raw := range []string{"0", "-1", "8h"} {
		if _, err := deepwiki.ConfigFromEnv(env(map[string]string{deepwiki.CallbackTokenTTLEnv: raw})); err == nil {
			t.Fatalf("TTL %q was accepted", raw)
		}
	}
}
