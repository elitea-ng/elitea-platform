package system_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
)

const indexBindingCrossProcessOptIn = "ELITEA_INDEX_BINDING_CROSS_PROCESS_TEST"

type indexBindingToolkitReader struct{}

func (indexBindingToolkitReader) GetCurrentToolkit(
	_ context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	if projectID != 42 || userID != 7 || toolkitID != 19 {
		return indexingapp.CurrentToolkitSnapshot{}, false, nil
	}
	return indexingapp.CurrentToolkitSnapshot{
		ID:   toolkitID,
		Type: "github",
		Name: "github-current",
		Settings: map[string]any{
			"credential": "secret-ref://github-current",
		},
	}, true, nil
}

type indexBindingModelCatalog struct {
	// ownerProjectID is the project that owns the authoritative default
	// embedding model: the shared public project, or the caller's own project.
	ownerProjectID   int32
	embeddingQueries []configurationapp.CurrentModelCatalogQuery
}

func (c *indexBindingModelCatalog) Get(
	_ context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	if query.Section != configurationapp.CurrentModelSectionEmbedding {
		return configurationapp.CurrentModelCatalogResponse{Items: []configurationapp.CurrentModelCatalogItem{}}, nil
	}
	c.embeddingQueries = append(c.embeddingQueries, query)
	name := "embedding-current"
	ownerProjectID := c.ownerProjectID
	return configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{{
			Name:      name,
			ProjectID: ownerProjectID,
			Shared:    ownerProjectID == 1,
			Default:   true,
		}},
		DefaultModelName:      &name,
		DefaultModelProjectID: &ownerProjectID,
	}, nil
}

type indexBindingSettingsResolver struct{}

func (indexBindingSettingsResolver) Resolve(
	_ context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	if request.ProjectID != 42 ||
		request.UserID != 7 ||
		request.ToolkitType != "github" ||
		request.Mode != configurationapp.CurrentToolkitSettingsReferenceMode {
		return nil, fmt.Errorf("unexpected settings request: %+v", request)
	}
	return map[string]any{
		"credential": "secret-ref://github-current",
	}, nil
}

type indexBindingConfigurationReader struct {
	// ownerProjectID is the only project holding the embedding row, and its
	// uuid differs per owner so a binding that bound the wrong scope carries a
	// different configuration identity and digest.
	ownerProjectID int32
	calls          []indexBindingConfigurationCall
}

type indexBindingConfigurationCall struct {
	projectID  int32
	modelName  string
	sharedOnly bool
}

func (r *indexBindingConfigurationReader) FindCurrentEmbeddingConfiguration(
	_ context.Context,
	projectID int32,
	modelName string,
	sharedOnly bool,
) (indexingapp.CurrentEmbeddingConfiguration, bool, error) {
	r.calls = append(r.calls, indexBindingConfigurationCall{
		projectID:  projectID,
		modelName:  modelName,
		sharedOnly: sharedOnly,
	})
	shared := r.ownerProjectID == 1
	if projectID != r.ownerProjectID || modelName != "embedding-current" ||
		sharedOnly != shared {
		return indexingapp.CurrentEmbeddingConfiguration{}, false, nil
	}
	return indexingapp.CurrentEmbeddingConfiguration{
		UUID:      fmt.Sprintf("00000000-0000-0000-0000-0000000001%02d", r.ownerProjectID),
		ProjectID: r.ownerProjectID,
		Type:      "embedding_model",
		Section:   "embedding",
		Data:      json.RawMessage(`{"name":"embedding-current"}`),
		Shared:    shared,
	}, true, nil
}

type indexBindingNoopAppender struct{}

func (indexBindingNoopAppender) Append(
	context.Context,
	string,
	string,
	string,
	[]byte,
) (string, error) {
	return "", fmt.Errorf("cross-process binding test must not append to Redis")
}

type indexBindingCrossProcessVector struct {
	Name                    string          `json:"name"`
	SignedV2                string          `json:"signed_v2"`
	SignedV1                string          `json:"signed_v1,omitempty"`
	PublicKey               string          `json:"public_key"`
	KeyID                   string          `json:"key_id"`
	Toolkit                 json.RawMessage `json:"toolkit"`
	Binding                 json.RawMessage `json:"binding"`
	BindingRaw              string          `json:"binding_raw"`
	ExpectedModelName       string          `json:"expected_model_name"`
	ExpectedGroup           string          `json:"expected_group"`
	ExpectedRoute           string          `json:"expected_route"`
	ExpectedModelProjectID  int32           `json:"expected_model_project_id"`
	ExpectedConfigProjectID int32           `json:"expected_config_project_id"`
}

type indexBindingCrossProcessResult struct {
	V1RejectedBeforeClaim bool     `json:"v1_rejected_before_claim"`
	V2ClaimCalls          int      `json:"v2_claim_calls"`
	AcceptedRoutes        []string `json:"accepted_routes"`
}

// TestIndexEmbeddingBindingMainWorkerCrossProcess proves the migration
// boundary with two real language processes. Main resolves the authoritative
// default (model, owner project) tuple and binds the Configurations row that
// backs it — the same row the Bifrost gateway pulls per project at request
// time, which is why the admitted wire name is the plain model name and not a
// LiteLLM-style `{projectID}_` group. The production Go producer
// signs only a reference command. The real Python delivery processor verifies
// the Ed25519 signature, rejects a correctly signed stale capability v1 before
// claim, and reaches claim for v2. The worker then validates the claim-scoped
// binding content without receiving credentials, endpoints or deployment
// policy in the control message.
func TestIndexEmbeddingBindingMainWorkerCrossProcess(t *testing.T) {
	if os.Getenv(indexBindingCrossProcessOptIn) != "1" {
		t.Skip("set ELITEA_INDEX_BINDING_CROSS_PROCESS_TEST=1 to run the Go-to-Python embedding-binding gate")
	}
	repositoryRoot := findRepositoryRoot(t)
	python := systemPython(t, repositoryRoot)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "index-binding-cross-process-key"
	signer, err := redisdispatch.NewEd25519CommandSigner(keyID, privateKey)
	if err != nil {
		t.Fatal(err)
	}

	// The model group prefix is gone with LiteLLM: every binding now carries the
	// plain model name, and the project scope survives only in the bound
	// configuration identity. The scenarios therefore vary the project that owns
	// the authoritative default model, not a proxy registry.
	scenarios := []struct {
		name                 string
		ownerProjectID       int32
		expectedSharedOnly   bool
		expectedConfigUUID   string
		expectedConfigDigest bool
	}{
		{
			name:               "public shared default",
			ownerProjectID:     1,
			expectedSharedOnly: true,
			expectedConfigUUID: "00000000-0000-0000-0000-000000000101",
		},
		{
			name:               "project owned default",
			ownerProjectID:     42,
			expectedSharedOnly: false,
			expectedConfigUUID: "00000000-0000-0000-0000-000000000142",
		},
	}
	vectors := make([]indexBindingCrossProcessVector, 0, len(scenarios))
	for index, scenario := range scenarios {
		catalog := &indexBindingModelCatalog{ownerProjectID: scenario.ownerProjectID}
		configurations := &indexBindingConfigurationReader{ownerProjectID: scenario.ownerProjectID}
		embeddingResolver, resolverErr := indexingapp.NewCurrentEmbeddingBindingResolver(
			configurations,
			1,
		)
		if resolverErr != nil {
			t.Fatal(resolverErr)
		}
		resolver, resolverErr := indexingapp.NewCurrentAuthoritativeInputResolver(
			indexBindingToolkitReader{},
			catalog,
			indexBindingSettingsResolver{},
			embeddingResolver,
			1,
		)
		if resolverErr != nil {
			t.Fatal(resolverErr)
		}
		inputs, resolveErr := resolver.Resolve(context.Background(), indexingapp.StartRequest{
			ProjectID:            42,
			ActorUserID:          7,
			ToolkitID:            19,
			RequestedLLMSettings: json.RawMessage(`{}`),
			ToolParameters:       json.RawMessage(`{"index_name":"cross-process"}`),
		})
		if resolveErr != nil {
			t.Fatalf("%s resolve: %v", scenario.name, resolveErr)
		}
		if inputs.EmbeddingBinding == nil ||
			inputs.EmbeddingBinding.ModelName != "embedding-current" ||
			inputs.EmbeddingBinding.ModelProjectID != scenario.ownerProjectID ||
			inputs.EmbeddingBinding.ConfigurationProjectID != scenario.ownerProjectID ||
			inputs.EmbeddingBinding.ConfigurationUUID != scenario.expectedConfigUUID ||
			inputs.EmbeddingBinding.ConfigurationDigest.IsZero() ||
			inputs.EmbeddingBinding.ResolvedModelGroup != "embedding-current" ||
			inputs.EmbeddingBinding.Route != "raw" {
			t.Fatalf("%s binding=%+v", scenario.name, inputs.EmbeddingBinding)
		}
		expectedCatalogQuery := configurationapp.CurrentModelCatalogQuery{
			Section:         configurationapp.CurrentModelSectionEmbedding,
			ProjectID:       42,
			PublicProjectID: 1,
			IncludeShared:   true,
		}
		if !reflect.DeepEqual(catalog.embeddingQueries, []configurationapp.CurrentModelCatalogQuery{expectedCatalogQuery}) {
			t.Fatalf("%s embedding catalog queries=%+v", scenario.name, catalog.embeddingQueries)
		}
		// Exactly one configuration search backs the whole binding now.
		if !reflect.DeepEqual(configurations.calls, []indexBindingConfigurationCall{{
			projectID:  scenario.ownerProjectID,
			modelName:  "embedding-current",
			sharedOnly: scenario.expectedSharedOnly,
		}}) {
			t.Fatalf("%s configuration owner lookups=%+v", scenario.name, configurations.calls)
		}

		bundle, binding, buildErr := buildIndexBindingCrossProcessBundle(inputs)
		if buildErr != nil {
			t.Fatalf("%s build input bundle: %v", scenario.name, buildErr)
		}
		bindingContent := indexBindingEntryContent(t, bundle)
		toolkitContent := indexBindingToolkitContent(t, bundle)
		v2 := prepareIndexBindingCrossProcessEnvelope(
			t,
			signer,
			bundle,
			binding,
			"2",
			fmt.Sprintf("%d-v2", index+1),
		)
		vector := indexBindingCrossProcessVector{
			Name:                    scenario.name,
			SignedV2:                base64.StdEncoding.EncodeToString(v2),
			PublicKey:               base64.StdEncoding.EncodeToString(publicKey),
			KeyID:                   keyID,
			Toolkit:                 toolkitContent,
			Binding:                 bindingContent,
			BindingRaw:              base64.StdEncoding.EncodeToString(bindingContent),
			ExpectedModelName:       "embedding-current",
			ExpectedGroup:           "embedding-current",
			ExpectedRoute:           "raw",
			ExpectedModelProjectID:  scenario.ownerProjectID,
			ExpectedConfigProjectID: scenario.ownerProjectID,
		}
		if index == 0 {
			vector.SignedV1 = base64.StdEncoding.EncodeToString(
				prepareIndexBindingCrossProcessEnvelope(
					t,
					signer,
					bundle,
					binding,
					"1",
					"stale-v1",
				),
			)
		}
		vectors = append(vectors, vector)
	}

	inputPath := filepath.Join(t.TempDir(), "index-binding-vectors.json")
	encoded, err := json.Marshal(vectors)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-c", indexBindingWorkerVerifier, inputPath)
	command.Env = append(os.Environ(), "PYTHONPATH="+pythonPath(repositoryRoot))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python worker cross-process verification failed: %v\n%s", err, output)
	}
	var result indexBindingCrossProcessResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Python worker result: %v\n%s", err, output)
	}
	wantRoutes := make([]string, 0, len(scenarios))
	for range scenarios {
		wantRoutes = append(wantRoutes, "raw")
	}
	if !result.V1RejectedBeforeClaim ||
		result.V2ClaimCalls != len(scenarios) ||
		!reflect.DeepEqual(result.AcceptedRoutes, wantRoutes) {
		t.Fatalf("unexpected Python worker result: %+v", result)
	}
}

func buildIndexBindingCrossProcessBundle(
	inputs indexingapp.AuthoritativeInputs,
) (executiondomain.InputBundle, executiondomain.IndexIngestBinding, error) {
	sequence := 0
	factory, err := indexingapp.NewInputBundleFactory(indexingapp.InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, func() (string, error) {
		sequence++
		return fmt.Sprintf("index-binding-input-%d", sequence), nil
	})
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.IndexIngestBinding{}, err
	}
	return factory.Build(context.Background(), inputs)
}

func prepareIndexBindingCrossProcessEnvelope(
	t *testing.T,
	signer redisdispatch.CommandSigner,
	bundle executiondomain.InputBundle,
	binding executiondomain.IndexIngestBinding,
	capabilityVersion string,
	suffix string,
) []byte {
	t.Helper()
	producer, err := redisdispatch.NewIndexIngestProducer(
		redisdispatch.IndexIngestProducerConfig{
			Stream:                 "commands.v1.index.ingest.indexing.shared.1.0",
			ConsumerGroup:          "elitea-indexer-worker-v1",
			ValidationStream:       "commands.v1.configuration.validate.short-validation.1.0",
			ProtocolRevision:       "elitea.runtime.v1",
			EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
			CapabilityVersion:      capabilityVersion,
			Limits: redisdispatch.Limits{
				Revision:               "elitea.runtime.limits.conformance.v2",
				MaxWorkerCommandBytes:  32 * 1024,
				MaxSignedEnvelopeBytes: 48 * 1024,
				MaxRedisFieldBytes:     48 * 1024,
				MaxRedisEntryBytes:     (64 * 1024) - 1,
				MaxSignatureBytes:      ed25519.SignatureSize,
				MaxStringBytes:         256,
			},
		},
		signer,
		indexBindingNoopAppender{},
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := producer.PrepareIndexIngest(context.Background(), indexingapp.IndexIngestDispatch{
		OutboxID:                    "index-binding-outbox-" + suffix,
		CommandID:                   "index-binding-command-" + suffix,
		ExecutionID:                 "index-binding-execution-" + suffix,
		Generation:                  1,
		DispatchOrdinal:             1,
		TenantID:                    "tenant-1",
		ResourceProjectID:           "42",
		ProjectionProjectID:         "42",
		PrincipalRef:                "user:7",
		InputBundleID:               bundle.ID,
		InputBundleVersion:          bundle.Version,
		InputBundleMediaType:        bundle.MediaType,
		InputBundleByteLength:       uint64(len(bundle.Manifest)),
		InputBundleDigest:           bundle.Digest,
		CapabilityVersion:           capabilityVersion,
		ResourceClass:               "indexing",
		IsolationClass:              "shared",
		Priority:                    1,
		Deadline:                    time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC),
		LimitsRevision:              "elitea.runtime.limits.conformance.v2",
		ToolkitConfigurationEntryID: binding.ToolkitConfigurationEntryID,
		ToolParametersEntryID:       binding.ToolParametersEntryID,
		EmbeddingBindingEntryID:     binding.EmbeddingBindingEntryID,
		EmbeddingBindingDigest:      binding.EmbeddingBindingDigest,
		SIOEvent:                    indexingapp.CurrentIndexSIOEvent,
		Initiator:                   executiondomain.IndexIngestInitiatorUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"embedding-current",
		"secret-ref://github-current",
		"resolved_model_group",
		"configuration_digest",
		"api_key",
		"endpoint",
		"deployment",
	} {
		if strings.Contains(string(prepared.Bytes), forbidden) {
			t.Fatalf("reference-only control envelope contains %q", forbidden)
		}
	}
	return prepared.Bytes
}

func indexBindingEntryContent(t *testing.T, bundle executiondomain.InputBundle) json.RawMessage {
	t.Helper()
	for _, entry := range bundle.Entries {
		if entry.SemanticRole == executiondomain.IndexEmbeddingBindingRole {
			return append(json.RawMessage(nil), entry.Content...)
		}
	}
	t.Fatal("embedding binding content is absent")
	return nil
}

func indexBindingToolkitContent(t *testing.T, bundle executiondomain.InputBundle) json.RawMessage {
	t.Helper()
	for _, entry := range bundle.Entries {
		if entry.SemanticRole == executiondomain.IndexToolkitConfigurationRole {
			return append(json.RawMessage(nil), entry.Content...)
		}
	}
	t.Fatal("toolkit configuration content is absent")
	return nil
}

const indexBindingWorkerVerifier = `
import asyncio
import base64
import hashlib
import json
import sys

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from elitea_worker.execution.delivery import IndexIngestDeliveryProcessor
from elitea_worker.execution.errors import UnsupportedCapability
from elitea_worker.handlers.indexing import IndexIngestInputBinding, ResolvedIndexIngestInput
from elitea_worker.protocol.codec import Ed25519CommandAuthenticator, parse_and_verify_signed_command
from elitea_worker.protocol.indexing import resolve_embedding_binding
from elitea_worker.transport.redis_commands import RedisCommandDelivery


class Resolver:
    def __init__(self, key_id, public_key):
        self.key_id = key_id
        self.public_key = Ed25519PublicKey.from_public_bytes(public_key)

    def resolve_ed25519_public_key(self, key_id):
        if key_id != self.key_id:
            raise KeyError(key_id)
        return self.public_key


class Runner:
    async def run(self, operation):
        return await operation()


class ReachedClaim(Exception):
    pass


class Control:
    def __init__(self):
        self.claims = 0

    async def claim_command(self, request):
        self.claims += 1
        raise ReachedClaim()


def processor(authenticator, control):
    return IndexIngestDeliveryProcessor(
        supervisor=Runner(),
        client_context_factory=None,
        control=control,
        command_acker=object(),
        input_client=object(),
        input_request_builder=object(),
        output_session_factory=lambda: None,
        signed_command_authenticator=authenticator,
        workload_session_id="index-binding-worker-session",
        producer_id="index-binding-main-producer",
        clock_unix_millis=lambda: 1_700_000_000_000,
    )


def delivery(raw):
    return RedisCommandDelivery(
        stream="commands.v1.index.ingest.indexing.shared.1.0",
        entry_id="1-0",
        fields={"signed_envelope": raw},
    )


async def main(path):
    with open(path, "r", encoding="utf-8") as source:
        vectors = json.load(source)
    accepted_routes = []
    v2_claims = 0
    v1_rejected_before_claim = False
    for vector in vectors:
        authenticator = Ed25519CommandAuthenticator(
            Resolver(
                vector["key_id"],
                base64.b64decode(vector["public_key"], validate=True),
            )
        )
        if vector.get("signed_v1"):
            control = Control()
            try:
                await processor(authenticator, control).process(
                    delivery(base64.b64decode(vector["signed_v1"], validate=True))
                )
            except UnsupportedCapability:
                v1_rejected_before_claim = control.claims == 0
            else:
                raise AssertionError("capability v1 reached the worker claim boundary")

        signed_raw = base64.b64decode(vector["signed_v2"], validate=True)
        control = Control()
        try:
            await processor(authenticator, control).process(delivery(signed_raw))
        except ReachedClaim:
            pass
        else:
            raise AssertionError("capability v2 did not reach the worker claim boundary")
        if control.claims != 1:
            raise AssertionError("capability v2 crossed claim more than once")
        v2_claims += control.claims

        _, command = parse_and_verify_signed_command(
            signed_raw,
            authenticator=authenticator,
        )
        binding_ref = command.index_ingest.embedding_binding
        binding_raw = base64.b64decode(vector["binding_raw"], validate=True)
        toolkit_raw = json.dumps(
            vector["toolkit"],
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        binding_digest = hashlib.sha256(binding_raw).digest()
        if (
            bytes(binding_ref.content_digest.value) != binding_digest
            or binding_ref.immutable_version
            != f"sha256:{binding_digest.hex()}"
            or json.loads(binding_raw) != vector["binding"]
        ):
            raise AssertionError("worker binding reference does not match Main content")
        binding = resolve_embedding_binding(
            ResolvedIndexIngestInput(
                binding=IndexIngestInputBinding(
                    entry_id=command.index_ingest.toolkit_configuration_entry_id,
                    immutable_version="toolkit",
                    content_digest=b"",
                ),
                value=vector["toolkit"],
            ),
            ResolvedIndexIngestInput(
                binding=IndexIngestInputBinding(
                    entry_id=binding_ref.entry_id,
                    immutable_version=binding_ref.immutable_version,
                    content_digest=bytes(binding_ref.content_digest.value),
                ),
                value=vector["binding"],
            ),
        )
        if binding is None:
            raise AssertionError("binding-aware capability v2 lost its binding")
        document = vector["binding"]
        if (
            binding.model_name != vector["expected_model_name"]
            or binding.resolved_model_group != vector["expected_group"]
            or document["route"] != vector["expected_route"]
            or document["model_project_id"] != vector["expected_model_project_id"]
            or document["configuration_project_id"]
            != vector["expected_config_project_id"]
        ):
            raise AssertionError("worker changed Main's admitted binding observation")
        forbidden_keys = {
            "api_key",
            "credential",
            "credentials",
            "endpoint",
            "api_base",
            "deployment",
            "provider",
        }
        if forbidden_keys.intersection(document):
            raise AssertionError("binding copied credential or deployment policy")
        for forbidden in (
            vector["expected_model_name"].encode(),
            vector["expected_group"].encode(),
            b"secret-ref://github-current",
            b"configuration_digest",
        ):
            if forbidden in signed_raw:
                raise AssertionError("control message copied content-plane model material")
        if not binding_raw or not toolkit_raw:
            raise AssertionError("claim-scoped content fixtures are empty")
        accepted_routes.append(document["route"])
    print(
        json.dumps(
            {
                "v1_rejected_before_claim": v1_rejected_before_claim,
                "v2_claim_calls": v2_claims,
                "accepted_routes": accepted_routes,
            },
            sort_keys=True,
        )
    )


asyncio.run(main(sys.argv[1]))
`

var _ indexingapp.CurrentToolkitReader = indexBindingToolkitReader{}
var _ indexingapp.CurrentModelCatalog = (*indexBindingModelCatalog)(nil)
var _ indexingapp.CurrentToolkitSettingsValidator = indexBindingSettingsResolver{}
var _ indexingapp.CurrentEmbeddingConfigurationReader = (*indexBindingConfigurationReader)(nil)
var _ redisdispatch.StreamAppender = indexBindingNoopAppender{}
