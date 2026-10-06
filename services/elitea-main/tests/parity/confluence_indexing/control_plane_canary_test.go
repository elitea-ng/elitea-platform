package confluenceindexing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

const (
	threeMiB      = 3 << 20
	thirtyTwoMiB  = 32 << 20
	maxRedisBytes = (64 << 10) - 1
)

type testSigner struct{}

func (testSigner) SignWorkerCommand(_ context.Context, command []byte) (commandbus.Signature, error) {
	digest := sha256.Sum256(command)
	return commandbus.Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyID:   "confluence-parity",
		Value:   digest[:],
	}, nil
}

type recordingAppender struct {
	value []byte
}

func (a *recordingAppender) Append(_ context.Context, _, _ string, value []byte) (string, error) {
	a.value = append([]byte(nil), value...)
	return "1-0", nil
}

func TestConfluenceBulkAndImageBytesNeverEnterRedis(t *testing.T) {
	t.Parallel()

	payloadDigest, canaries := confluenceProductionScaleDigest()
	appender := &recordingAppender{}
	producer, err := commandbus.NewIndexIngestProducer(
		commandbus.IndexIngestProducerConfig{
			Stream:                 "commands.v1.index.ingest.parity",
			Consumer:               "workers.v1.index.ingest.parity",
			ValidationStream:       "commands.v1.configuration.validate",
			ProtocolRevision:       "runtime.v1",
			EnvelopeSchemaRevision: "signed-worker-command.v1",
			CapabilityVersion:      "1",
			AllowTestOnlyHMAC:      true,
			Limits: commandbus.Limits{
				Revision:                 "parity-limits-v1",
				MaxWorkerCommandBytes:    16 << 10,
				MaxSignedEnvelopeBytes:   32 << 10,
				MaxTransportPayloadBytes: 48 << 10,
				MaxTransportMessageBytes: maxRedisBytes,
				MaxSignatureBytes:        256,
				MaxStringBytes:           4096,
			},
		},
		testSigner{},
		appender,
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := indexingapp.IndexIngestDispatch{
		OutboxID:                    "outbox-confluence-scale",
		CommandID:                   "command-confluence-scale",
		ExecutionID:                 "execution-confluence-scale",
		Generation:                  1,
		DispatchOrdinal:             1,
		TenantID:                    "tenant-1",
		ResourceProjectID:           "1",
		ProjectionProjectID:         "1",
		PrincipalRef:                "principal-1",
		InputBundleID:               "bundle-confluence-scale",
		InputBundleVersion:          "immutable-v1",
		InputBundleMediaType:        "application/vnd.elitea.input-bundle.v1+json",
		InputBundleByteLength:       10*threeMiB + thirtyTwoMiB,
		InputBundleDigest:           payloadDigest,
		CapabilityVersion:           "1",
		ResourceClass:               "indexing",
		IsolationClass:              "project",
		Priority:                    1,
		Deadline:                    time.Unix(1_900_000_000, 0).UTC(),
		LimitsRevision:              "parity-limits-v1",
		ToolkitConfigurationEntryID: "entry-toolkit",
		ToolParametersEntryID:       "entry-parameters",
		LLMModelEntryID:             "entry-vision-model",
		LLMConfigurationEntryID:     "entry-model-configuration",
		ClientStreamID:              "stream-confluence-scale",
		ClientMessageID:             "message-confluence-scale",
		SIOEvent:                    "chat_predict",
		Initiator:                   executiondomain.IndexIngestInitiatorUser,
	}
	prepared, err := producer.PrepareIndexIngest(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	if len(appender.value) == 0 || len(appender.value) >= maxRedisBytes {
		t.Fatalf("unexpected Redis envelope size: %d", len(appender.value))
	}
	for _, canary := range canaries {
		if bytes.Contains(appender.value, canary) {
			t.Fatalf("Redis envelope contains Confluence data-plane canary %q", canary)
		}
	}
	for _, fragment := range [][]byte{
		[]byte("data:image/png;base64,"),
		[]byte("PARENT_IMAGE_DESCRIPTION"),
		[]byte("DEPENDENT_IMAGE_DESCRIPTION"),
		[]byte("release notes from the text attachment"),
	} {
		if bytes.Contains(appender.value, fragment) {
			t.Fatalf("Redis envelope contains forbidden Confluence fragment %q", fragment)
		}
	}
	if got := dispatch.InputBundleByteLength; got != 62<<20 {
		t.Fatalf("production-scale fixture size = %d, want %d", got, 62<<20)
	}
}

func confluenceProductionScaleDigest() (runtimedomain.Digest, [][]byte) {
	hash := sha256.New()
	canaries := make([][]byte, 0, 11)
	for index := 0; index < 10; index++ {
		canary := []byte(fmt.Sprintf("CONFLUENCE_3MIB_IMAGE_%02d_BASE64_CANARY", index))
		canaries = append(canaries, canary)
		writeRepeated(hash, canary, threeMiB)
	}
	large := []byte("CONFLUENCE_32MIB_IMAGE_BASE64_CANARY")
	canaries = append(canaries, large)
	writeRepeated(hash, large, thirtyTwoMiB)
	sum := hash.Sum(nil)
	var digest runtimedomain.Digest
	copy(digest[:], sum)
	return digest, canaries
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writeRepeated(writer byteWriter, pattern []byte, size int) {
	remaining := size
	for remaining > 0 {
		chunk := pattern
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		_, _ = writer.Write(chunk)
		remaining -= len(chunk)
	}
}
