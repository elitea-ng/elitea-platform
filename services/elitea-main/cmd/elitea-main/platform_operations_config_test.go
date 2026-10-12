package main

import (
	"strings"
	"testing"
	"time"

	v2deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

var (
	enabledDeepWiki = v2deepwiki.Config{Enabled: true, ClientCertFile: "/c/client.crt",
		ClientKeyFile: "/c/client.key", CAFile: "/c/ca.crt", ServerName: "elitea-deepwiki", Timeout: 30 * time.Second}
	enabledInventory = facade.Config{Enabled: true, ClientCertFile: "/c/client.crt",
		ClientKeyFile: "/c/client.key", CAFile: "/c/ca.crt", Timeout: 5 * time.Minute}
)

func TestNoTargetsMeansNoClients(t *testing.T) {
	t.Parallel()
	configs, err := platformOperationsConfigs(enabledDeepWiki, enabledInventory, lookupFrom(nil))
	if err != nil || len(configs) != 0 {
		t.Fatalf("configs=%v err=%v", configs, err)
	}
}

func TestATargetReusesItsFacadesMaterial(t *testing.T) {
	t.Parallel()
	configs, err := platformOperationsConfigs(enabledDeepWiki, enabledInventory, lookupFrom(map[string]string{
		deepwikiPlatformTargetEnv:  " elitea-deepwiki:9443 ",
		inventoryPlatformTargetEnv: "elitea-inventory:9443",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs = %+v", configs)
	}
	deepwiki, inventory := configs[0], configs[1]
	if deepwiki.App != platformops.AppDeepWiki || deepwiki.Target != "elitea-deepwiki:9443" ||
		deepwiki.ClientCertFile != "/c/client.crt" || deepwiki.ServerName != "elitea-deepwiki" {
		t.Errorf("deepwiki = %+v", deepwiki)
	}
	// The facade's 30s is for a proxy hop; a deletion waits for the engine to
	// stop its runs first, so the bound never drops below the package default.
	if deepwiki.CallTimeout != platformops.DefaultCallTimeout {
		t.Errorf("deepwiki call timeout = %s", deepwiki.CallTimeout)
	}
	if inventory.App != platformops.AppInventory || inventory.CAFile != "/c/ca.crt" || inventory.CallTimeout != 5*time.Minute {
		t.Errorf("inventory = %+v", inventory)
	}
}

func TestATargetBesideADisabledFacadeIsABootError(t *testing.T) {
	t.Parallel()
	_, err := platformOperationsConfigs(v2deepwiki.Config{}, enabledInventory, lookupFrom(map[string]string{
		deepwikiPlatformTargetEnv: "elitea-deepwiki:9443",
	}))
	if err == nil || !strings.Contains(err.Error(), deepwikiPlatformTargetEnv) || !strings.Contains(err.Error(), v2deepwiki.EnabledEnv) {
		t.Fatalf("err = %v, want one naming both variables", err)
	}
	_, err = platformOperationsConfigs(enabledDeepWiki, facade.Config{}, lookupFrom(map[string]string{
		inventoryPlatformTargetEnv: "elitea-inventory:9443",
	}))
	if err == nil || !strings.Contains(err.Error(), inventoryPlatformTargetEnv) {
		t.Fatalf("err = %v", err)
	}
}

func TestAMalformedTargetIsRefused(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"https://elitea-deepwiki:9443", "elitea-deepwiki", ":9443"} {
		if _, err := platformOperationsConfigs(enabledDeepWiki, enabledInventory, lookupFrom(map[string]string{
			deepwikiPlatformTargetEnv: target,
		})); err == nil {
			t.Errorf("target %q was accepted", target)
		}
	}
}
