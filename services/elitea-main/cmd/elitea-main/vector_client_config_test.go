package main

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/vectorclient"
)

var completeVectorEnv = map[string]string{
	vectorTargetEnv:     " elitea-vector:9470 ",
	vectorClientCertEnv: "/run/elitea-vector-client/tls.crt",
	vectorClientKeyEnv:  "/run/elitea-vector-client/tls.key",
	vectorCAFileEnv:     "/run/elitea-vector-client/runtime-ca.crt",
}

func TestNoVectorTargetMeansNoClient(t *testing.T) {
	t.Parallel()
	if _, err := vectorClientConfig(lookupFrom(nil)); !errors.Is(err, errNoVectorClient) {
		t.Fatalf("err = %v, want errNoVectorClient", err)
	}
}

func TestVectorMaterialBesideNoTargetIsRefused(t *testing.T) {
	t.Parallel()
	for _, key := range []string{vectorClientCertEnv, vectorClientKeyEnv, vectorCAFileEnv, vectorServerNameEnv} {
		_, err := vectorClientConfig(lookupFrom(map[string]string{key: "/x"}))
		if err == nil || errors.Is(err, errNoVectorClient) || !strings.Contains(err.Error(), key) {
			t.Errorf("%s set alone: err = %v, want a refusal naming it", key, err)
		}
	}
}

func TestACompleteVectorConfigIsReadAndTrimmed(t *testing.T) {
	t.Parallel()
	cfg, err := vectorClientConfig(lookupFrom(completeVectorEnv))
	if err != nil {
		t.Fatal(err)
	}
	want := vectorclient.Config{
		Target: "elitea-vector:9470", ClientCertFile: "/run/elitea-vector-client/tls.crt",
		ClientKeyFile: "/run/elitea-vector-client/tls.key", CAFile: "/run/elitea-vector-client/runtime-ca.crt",
	}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
	withName := map[string]string{vectorServerNameEnv: "elitea-vector.platform.svc"}
	for key, value := range completeVectorEnv {
		withName[key] = value
	}
	cfg, err = vectorClientConfig(lookupFrom(withName))
	if err != nil || cfg.ServerName != "elitea-vector.platform.svc" {
		t.Fatalf("server name: cfg=%+v err=%v", cfg, err)
	}
}

func TestAVectorTargetWithoutItsMaterialIsABootError(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{vectorClientCertEnv, vectorClientKeyEnv, vectorCAFileEnv} {
		env := map[string]string{}
		for key, value := range completeVectorEnv {
			if key != missing {
				env[key] = value
			}
		}
		_, err := vectorClientConfig(lookupFrom(env))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: err = %v, want a refusal naming it", missing, err)
		}
	}
	relative := map[string]string{}
	for key, value := range completeVectorEnv {
		relative[key] = value
	}
	relative[vectorCAFileEnv] = "runtime-ca.crt"
	if _, err := vectorClientConfig(lookupFrom(relative)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path: err = %v", err)
	}
	url := map[string]string{}
	for key, value := range completeVectorEnv {
		url[key] = value
	}
	url[vectorTargetEnv] = "https://elitea-vector:9470"
	if _, err := vectorClientConfig(lookupFrom(url)); !errors.Is(err, vectorclient.ErrConfig) {
		t.Errorf("url target: err = %v, want ErrConfig", err)
	}
}

func TestComposeVectorClientWithoutATargetIsNilAndLogged(t *testing.T) {
	t.Parallel()
	for _, rust := range []bool{false, true} {
		client, closeClient, err := composeVectorClient(slog.New(slog.NewTextHandler(io.Discard, nil)), lookupFrom(nil), rust)
		if err != nil || client != nil || closeClient == nil {
			t.Fatalf("rust=%v: client=%v close-nil=%v err=%v", rust, client, closeClient == nil, err)
		}
		closeClient()
	}
	// A target without its files is refused at boot, not composed and refused
	// on every call.
	if _, _, err := composeVectorClient(slog.New(slog.NewTextHandler(io.Discard, nil)), lookupFrom(map[string]string{vectorTargetEnv: "elitea-vector:9470"}), true); err == nil {
		t.Fatal("a target without material composed a client")
	}
	// Material that names files which do not exist is a boot error too.
	if _, _, err := composeVectorClient(slog.New(slog.NewTextHandler(io.Discard, nil)), lookupFrom(completeVectorEnv), true); !errors.Is(err, vectorclient.ErrConfig) {
		t.Fatalf("missing files: err = %v, want ErrConfig", err)
	}
}

// ELITEA_VECTOR_CLEANUP_ABANDON: default off, boolean forms, refused beside a
// client, a typo is a boot error rather than "false".
func TestTheVectorCleanupAbandonSwitch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, raw := range []string{"", " ", "false", "0", "FALSE"} {
		if on, err := vectorCleanupAbandonSwitch(logger, lookupFrom(map[string]string{vectorCleanupAbandonEnv: raw}), false); err != nil || on {
			t.Errorf("%q: on=%v err=%v, want off", raw, on, err)
		}
	}
	for _, raw := range []string{"true", " TRUE ", "1"} {
		if on, err := vectorCleanupAbandonSwitch(logger, lookupFrom(map[string]string{vectorCleanupAbandonEnv: raw}), false); err != nil || !on {
			t.Errorf("%q: on=%v err=%v, want on", raw, on, err)
		}
	}
	if _, err := vectorCleanupAbandonSwitch(logger, lookupFrom(map[string]string{vectorCleanupAbandonEnv: "yes please"}), false); err == nil {
		t.Error("an unparsable value passed")
	}
	// Beside a client the switch is a contradiction and a boot error.
	if _, err := vectorCleanupAbandonSwitch(logger, lookupFrom(map[string]string{vectorCleanupAbandonEnv: "true"}), true); err == nil || !strings.Contains(err.Error(), vectorTargetEnv) {
		t.Errorf("abandon beside a client: err = %v, want a refusal naming %s", err, vectorTargetEnv)
	}
	// Off beside a client is fine.
	if on, err := vectorCleanupAbandonSwitch(logger, lookupFrom(map[string]string{}), true); err != nil || on {
		t.Errorf("unset beside a client: on=%v err=%v", on, err)
	}
}
