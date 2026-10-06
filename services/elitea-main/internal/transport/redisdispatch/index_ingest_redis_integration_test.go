package redisdispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// TestIndexIngestRedisServiceBackedDedicatedBoundedStream is an opt-in
// single-Redis service integration test. It proves dedicated stream/group
// routing, exact-delivery deduplication, reference-only bytes and atomic
// saturation. PostgreSQL outbox retention and worker processing are separate
// integration boundaries and are not claimed here.
func TestIndexIngestRedisServiceBackedDedicatedBoundedStream(t *testing.T) {
	address := os.Getenv(redisServiceTestAddressEnv)
	if address == "" {
		t.Skipf("set %s to run the real-Redis index dispatch test", redisServiceTestAddressEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{
		Addr:         address,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   0,
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Redis client: %v", err)
		}
	}()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis service at %s: %v", address, err)
	}

	suffix := time.Now().UnixNano()
	validationStream := fmt.Sprintf("elitea:test:validation:%d", suffix)
	indexStream := fmt.Sprintf("elitea:test:index-ingest:%d", suffix)
	validationGroup := "elitea-validation-worker-test"
	indexGroup := "elitea-indexer-worker-test"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		if err := client.Del(cleanupCtx, validationStream, deliveryIndexKey(validationStream), indexStream, deliveryIndexKey(indexStream)).Err(); err != nil {
			t.Errorf("delete Redis test routes: %v", err)
		}
	}()
	if err := client.XGroupCreateMkStream(ctx, validationStream, validationGroup, "0").Err(); err != nil {
		t.Fatalf("create validation consumer group: %v", err)
	}
	if err := client.XGroupCreateMkStream(ctx, indexStream, indexGroup, "0").Err(); err != nil {
		t.Fatalf("create index consumer group: %v", err)
	}

	config := validIndexIngestProducerConfig()
	config.Stream = indexStream
	config.ValidationStream = validationStream
	config.ConsumerGroup = indexGroup
	appender, err := NewRedisStreamAppender(client, RedisStreamAppenderConfig{
		MaxEntries:    1,
		MaxEntryBytes: config.Limits.MaxTransportMessageBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := NewIndexIngestProducer(config, redisServiceHMACSigner{}, appender)
	if err != nil {
		t.Fatal(err)
	}

	first := validIndexIngestCommand()
	firstPrepared, err := producer.Prepare(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, first.GetIdempotencyKey(), firstPrepared); err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, first.GetIdempotencyKey(), firstPrepared.Clone()); err != nil {
		t.Fatalf("deduplicate exact index delivery: %v", err)
	}

	second := proto.Clone(first).(*runtimev1.WorkerCommandV1)
	second.CommandId = "index-command-2"
	second.IdempotencyKey = "index-outbox-2"
	second.ExecutionId = "index-execution-2"
	second.RootExecutionId = second.ExecutionId
	second.InputBundleRef.InputBundleId = "index-bundle-2"
	secondPrepared, err := producer.Prepare(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, second.GetIdempotencyKey(), secondPrepared); !errors.Is(err, ErrControlStreamSaturated) || !errors.Is(err, executionapp.ErrDispatchBackpressured) {
		t.Fatalf("saturated index append error = %v", err)
	}
	if length, err := client.XLen(ctx, validationStream).Result(); err != nil || length != 0 {
		t.Fatalf("index delivery leaked to validation stream: length=%d err=%v", length, err)
	}
	if length, err := client.XLen(ctx, indexStream).Result(); err != nil || length != 1 {
		t.Fatalf("bounded index stream length=%d err=%v, want 1", length, err)
	}
	if mappings, err := client.HLen(ctx, deliveryIndexKey(indexStream)).Result(); err != nil || mappings != 1 {
		t.Fatalf("bounded index delivery mappings=%d err=%v, want 1", mappings, err)
	}

	message := readNewRedisMessage(t, ctx, client, indexStream, indexGroup, "index-consumer")
	value := assertReferenceOnlyRedisMessage(t, message, config.Limits, [][]byte{
		[]byte("CONFLUENCE_IMAGE_SOURCE_CANARY"),
		[]byte("TOOLKIT_CONFIGURATION_SECRET_CANARY"),
		[]byte("INDEX_RESULT_BODY_CANARY"),
	})
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(value, envelope); err != nil {
		t.Fatal(err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), command); err != nil {
		t.Fatal(err)
	}
	if command.GetCapabilityId() != indexIngestCapabilityID || command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST || command.GetIndexIngest() == nil {
		t.Fatalf("unexpected Redis index command: %+v", command)
	}
}
