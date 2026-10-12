package main

// elitea-main's client of elitea-vector (ADR-0031 decision 4): where the vector
// store's gRPC listener is and the mutual-TLS material this process presents
// to it. One client serves two deletions — a toolkit index's namespace (the
// index delete route and the registry's tombstone sweeper) and a project's
// every namespace (one step of the project delete's cleanup journal).
//
// FIVE VARIABLES, ALL-OR-NOTHING. ELITEA_VECTOR_GRPC_TARGET names the
// listener as host:port (compose: elitea-vector:9470; the chart derives
// <vector.service.name>:<vector.service.port>). With it set, the three
// material files are mandatory and a missing one is a boot error:
// elitea-vector terminates mTLS with a required client certificate and admits
// this process by that certificate's identity alone (its one DNS SAN, read as
// dns:<name>, listed in its ELITEA_VECTOR_ADMIN_IDENTITIES), so a client
// without material would be composed and refused on every call. The optional
// ELITEA_VECTOR_GRPC_SERVER_NAME overrides the name verified against
// elitea-vector's certificate (default: the target's host).
//
// THE MATERIAL IS NOT THE PROVIDER HOPS'. elitea-vector's listener trusts the
// RUNTIME CA (the one that signs the workers' identities and this process's
// own control, output and content listeners), not the CA of the DeepWiki and
// Inventory hops. So this hop has its own client certificate, issued by the
// runtime CA with one DNS SAN (deploy/scripts/gen-vector-certs.sh:
// main-client.crt; the chart: the elitea-main-vector-client Certificate), and
// its CA file is runtime-ca.crt.
//
// WITHOUT A TARGET the deployment has no vector client: index tombstones stay
// pending (indexingapp.DeferredIndexVectorDeleter, the sweeper idles) and a
// deleted project's vectors stay in Qdrant. Under the Rust indexing runtime
// that is the leak ADR-0031 decision 4 closes, and it is logged at Warn at
// boot; under the Python runtime no toolkit index is in elitea-vector, and
// the absence is logged at Info.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/vectorclient"
)

const (
	vectorTargetEnv     = "ELITEA_VECTOR_GRPC_TARGET"
	vectorClientCertEnv = "ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE"
	vectorClientKeyEnv  = "ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE"
	vectorCAFileEnv     = "ELITEA_VECTOR_GRPC_CA_FILE"
	vectorServerNameEnv = "ELITEA_VECTOR_GRPC_SERVER_NAME"
)

// errNoVectorClient reports that no target is set: the deployment has no
// elitea-vector client, which is allowed.
var errNoVectorClient = errors.New("no elitea-vector target configured")

// vectorClientConfig reads the five variables. It returns errNoVectorClient
// when the target is unset (and refuses material set beside no target, which
// is the state that looks configured and does nothing), and a validated
// Config otherwise.
func vectorClientConfig(lookup func(string) (string, bool)) (vectorclient.Config, error) {
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}
	cfg := vectorclient.Config{
		Target:         get(vectorTargetEnv),
		ClientCertFile: get(vectorClientCertEnv),
		ClientKeyFile:  get(vectorClientKeyEnv),
		CAFile:         get(vectorCAFileEnv),
		ServerName:     get(vectorServerNameEnv),
	}
	if cfg.Target == "" {
		for _, key := range []string{vectorClientCertEnv, vectorClientKeyEnv, vectorCAFileEnv, vectorServerNameEnv} {
			if get(key) != "" {
				return vectorclient.Config{}, fmt.Errorf("%s is set but %s is not: the elitea-vector material is named and never read. Set the target (host:port of elitea-vector's gRPC listener), or clear %s",
					key, vectorTargetEnv, key)
			}
		}
		return vectorclient.Config{}, errNoVectorClient
	}
	for _, file := range []struct{ env, value string }{
		{vectorClientCertEnv, cfg.ClientCertFile},
		{vectorClientKeyEnv, cfg.ClientKeyFile},
		{vectorCAFileEnv, cfg.CAFile},
	} {
		switch {
		case file.value == "":
			return vectorclient.Config{}, fmt.Errorf("%s is set, so %s must be set too: elitea-vector requires a client certificate issued by the runtime CA and admits this process by its identity (ELITEA_VECTOR_ADMIN_IDENTITIES); without the material every deletion would be refused",
				vectorTargetEnv, file.env)
		case !strings.HasPrefix(file.value, "/"):
			return vectorclient.Config{}, fmt.Errorf("%s must be an absolute file path, got %q", file.env, file.value)
		}
	}
	if err := cfg.Validate(); err != nil {
		return vectorclient.Config{}, fmt.Errorf("%s: %w", vectorTargetEnv, err)
	}
	return cfg, nil
}

// composeVectorClient builds the client, or returns nil when no target is set.
// rustIndexing says whether toolkit indexes live in elitea-vector, which
// decides how loudly the absence is logged. The returned close releases the
// connection.
func composeVectorClient(
	logger *slog.Logger,
	lookup func(string) (string, bool),
	rustIndexing bool,
) (*vectorclient.Client, func(), error) {
	cfg, err := vectorClientConfig(lookup)
	switch {
	case errors.Is(err, errNoVectorClient):
		if rustIndexing {
			logger.Warn("elitea-vector client is NOT wired: deleted indexes keep their vectors (tombstones stay pending) and deleted projects keep theirs in Qdrant for ever",
				"set", vectorTargetEnv)
		} else {
			logger.Info("elitea-vector client is not wired; the Python indexing runtime holds no toolkit index there",
				"set", vectorTargetEnv)
		}
		return nil, func() {}, nil
	case err != nil:
		return nil, nil, err
	}
	client, err := vectorclient.New(cfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("compose elitea-vector gRPC client: %w", err)
	}
	logger.Info("elitea-vector client is wired: deleted indexes and deleted projects lose their vectors",
		"target", cfg.Target, "rust_indexing", rustIndexing)
	return client, func() { _ = client.Close() }, nil
}
