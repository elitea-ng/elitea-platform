package redisdispatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/redis/go-redis/v9"
)

const redisServiceTestAddressEnv = "ELITEA_TEST_REDIS_ADDR"

// TestRedisStreamServiceBackedControlPlane exercises only the Redis control
// adapter against a real Redis 7 service. It is not a full runtime E2E test:
// PostgreSQL, the Python worker, gRPC, HTTPS input, and SSE are out of scope.
func TestRedisStreamServiceBackedControlPlane(t *testing.T) {
	address := os.Getenv(redisServiceTestAddressEnv)
	if address == "" {
		t.Skipf("set %s to run the real-Redis integration test", redisServiceTestAddressEnv)
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

	stream := fmt.Sprintf("elitea:test:configuration-validation:%d", time.Now().UnixNano())
	group := "elitea-worker-python-test"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		if err := client.Del(cleanupCtx, stream, deliveryIndexKey(stream)).Err(); err != nil {
			t.Errorf("delete Redis test stream: %v", err)
		}
	}()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("create Redis consumer group: %v", err)
	}

	limits, err := LimitsFromProto(&runtimev1.ProtocolLimitsV1{
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		MaxWorkerCommandBytes:  32 * 1024,
		MaxSignedEnvelopeBytes: 48 * 1024,
		MaxRedisFieldBytes:     48 * 1024,
		MaxRedisEntryBytes:     64 * 1024,
		MaxSafeStringBytes:     256,
	})
	if err != nil {
		t.Fatal(err)
	}
	appender, err := NewRedisStreamAppender(client, RedisStreamAppenderConfig{
		MaxEntries:    2,
		MaxEntryBytes: limits.MaxRedisEntryBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	config := validProducerConfig()
	config.Stream = stream
	config.ProtocolRevision = "elitea.runtime.v1"
	config.EnvelopeSchemaRevision = "elitea.runtime.signed-worker-command.v1"
	config.Limits = limits
	producer, err := NewProducer(config, redisServiceHMACSigner{}, appender)
	if err != nil {
		t.Fatal(err)
	}

	settingsCanary := []byte(`{"auth_type":"Digest","secret":"ELITEA_SETTINGS_CANARY"}`)
	outputCanary := []byte("ELITEA_OUTPUT_CANARY:binary-terminal-frame")
	heavyToken := []byte("ELITEA_32_MIB_BODY_CANARY")
	heavyBody := bytes.Repeat(heavyToken, (32<<20)/len(heavyToken)+1)
	heavyBody = heavyBody[:32<<20]
	if len(heavyBody) != 32<<20 {
		t.Fatal("test did not construct the 32 MiB regression canary")
	}
	forbidden := [][]byte{
		settingsCanary,
		[]byte("ELITEA_SETTINGS_CANARY"),
		outputCanary,
		[]byte("ELITEA_OUTPUT_CANARY"),
		heavyToken,
		heavyBody,
	}

	dispatch := validTransportDispatch()
	dispatch.CapabilityVersion = "1"
	dispatch.LimitsRevision = limits.Revision
	prepared, err := producer.PrepareValidation(ctx, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	// A second publisher/retry for the same stable delivery and exact durable
	// bytes must observe the existing entry instead of consuming capacity.
	if err := producer.AppendPrepared(ctx, dispatch.OutboxID, prepared.Clone()); err != nil {
		t.Fatalf("deduplicate exact prepared delivery: %v", err)
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 1 {
		t.Fatalf("exact delivery retry produced length=%d err=%v, want 1", length, err)
	}
	if mappings, err := client.HLen(ctx, deliveryIndexKey(stream)).Result(); err != nil || mappings != 1 {
		t.Fatalf("exact delivery retry produced mappings=%d err=%v, want 1", mappings, err)
	}
	first := readNewRedisMessage(t, ctx, client, stream, group, "consumer-primary")
	firstValue := assertReferenceOnlyRedisMessage(t, first, config.Limits, forbidden)
	if prepared.Digest != runtimedomain.SHA256(firstValue) {
		t.Fatal("producer digest does not bind the exact Redis field bytes")
	}
	assertPendingCount(t, ctx, client, stream, group, 1)
	claimed, _, err := client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   stream,
		Group:    group,
		Consumer: "consumer-recovery",
		MinIdle:  0,
		Start:    "0-0",
		Count:    1,
	}).Result()
	if err != nil {
		t.Fatalf("reclaim pending Redis delivery: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("unexpected reclaimed Redis deliveries: %+v", claimed)
	}
	assertReferenceOnlyRedisMessage(t, claimed[0], config.Limits, forbidden)

	pending, err := client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  1,
	}).Result()
	if err != nil {
		t.Fatalf("inspect reclaimed Redis delivery: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != first.ID || pending[0].Consumer != "consumer-recovery" {
		t.Fatalf("pending Redis delivery was not transferred to the recovery consumer: %+v", pending)
	}
	if acknowledged, err := client.XAck(ctx, stream, group, first.ID).Result(); err != nil {
		t.Fatalf("ack reclaimed Redis delivery: %v", err)
	} else if acknowledged != 1 {
		t.Fatalf("acknowledged %d reclaimed Redis deliveries, want 1", acknowledged)
	}
	if deleted, err := client.XDel(ctx, stream, first.ID).Result(); err != nil || deleted != 1 {
		t.Fatalf("delete reclaimed Redis delivery: deleted=%d err=%v", deleted, err)
	}
	if deleted, err := client.HDel(ctx, deliveryIndexKey(stream), dispatch.OutboxID).Result(); err != nil || deleted != 1 {
		t.Fatalf("delete reclaimed delivery mapping: deleted=%d err=%v", deleted, err)
	}
	assertPendingCount(t, ctx, client, stream, group, 0)

	prepareDelivery := func(suffix string) (executionapp.ValidationDispatch, executionapp.PreparedCommandEnvelope) {
		t.Helper()
		candidate := validTransportDispatch()
		candidate.OutboxID = "outbox-" + suffix
		candidate.CommandID = "command-" + suffix
		candidate.ExecutionID = "execution-" + suffix
		candidate.InputBundleID = "bundle-" + suffix
		candidate.Command.ConfigurationRevisionID = "revision-" + suffix
		candidate.CapabilityVersion = "1"
		candidate.LimitsRevision = limits.Revision
		encoded, err := producer.PrepareValidation(ctx, candidate)
		if err != nil {
			t.Fatal(err)
		}
		return candidate, encoded
	}
	secondDispatch, secondPrepared := prepareDelivery("second")
	thirdDispatch, thirdPrepared := prepareDelivery("third")
	if err := producer.AppendPrepared(ctx, dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, secondDispatch.OutboxID, secondPrepared); err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(ctx, thirdDispatch.OutboxID, thirdPrepared); !errors.Is(err, ErrControlStreamSaturated) {
		t.Fatalf("expected Redis stream saturation after two live entries, got %v", err)
	} else {
		var saturation *ControlStreamSaturatedError
		if !errors.As(err, &saturation) || saturation.CurrentEntries != 2 || saturation.CurrentMappings != 2 || saturation.MaxEntries != 2 {
			t.Fatalf("unexpected Redis saturation details: %#v", saturation)
		}
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 2 {
		t.Fatalf("saturated append changed Redis stream length: length=%d err=%v", length, err)
	}
	entries, err := client.XRangeN(ctx, stream, "-", "+", 1).Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("select first live Redis entry: entries=%v err=%v", entries, err)
	}
	if deleted, err := client.XDel(ctx, stream, entries[0].ID).Result(); err != nil || deleted != 1 {
		t.Fatalf("delete settled Redis entry: deleted=%d err=%v", deleted, err)
	}
	if deleted, err := client.HDel(ctx, deliveryIndexKey(stream), dispatch.OutboxID).Result(); err != nil || deleted != 1 {
		t.Fatalf("delete settled Redis delivery mapping: deleted=%d err=%v", deleted, err)
	}
	if err := producer.AppendPrepared(ctx, thirdDispatch.OutboxID, thirdPrepared); err != nil {
		t.Fatalf("append after XDEL released capacity: %v", err)
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 2 {
		t.Fatalf("Redis capacity recovery produced length=%d err=%v, want 2", length, err)
	}

	secondClient := redis.NewClient(&redis.Options{
		Addr:         address,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   0,
	})
	defer func() {
		if err := secondClient.Close(); err != nil {
			t.Errorf("close second Redis client: %v", err)
		}
	}()
	secondAppender, err := NewRedisStreamAppender(secondClient, RedisStreamAppenderConfig{
		MaxEntries:    2,
		MaxEntryBytes: limits.MaxRedisEntryBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondProducer, err := NewProducer(config, redisServiceHMACSigner{}, secondAppender)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Del(ctx, stream, deliveryIndexKey(stream)).Err(); err != nil {
		t.Fatalf("reset Redis stream before concurrent publishers: %v", err)
	}

	const concurrentAttempts = 32
	start := make(chan struct{})
	results := make(chan error, concurrentAttempts)
	producers := []*Producer{producer, secondProducer}
	var publishers sync.WaitGroup
	publishers.Add(concurrentAttempts)
	for index := range concurrentAttempts {
		go func() {
			defer publishers.Done()
			<-start
			results <- producers[index%len(producers)].AppendPrepared(ctx, dispatch.OutboxID, prepared.Clone())
		}()
	}
	close(start)
	publishers.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		switch err {
		case nil:
			succeeded++
		default:
			t.Fatalf("unexpected concurrent Redis append result: %v", err)
		}
	}
	if succeeded != concurrentAttempts {
		t.Fatalf("cross-publisher dedupe successes=%d, want %d", succeeded, concurrentAttempts)
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 1 {
		t.Fatalf("concurrent Redis publishers duplicated stable delivery: length=%d err=%v", length, err)
	}
	if mappings, err := client.HLen(ctx, deliveryIndexKey(stream)).Result(); err != nil || mappings != 1 {
		t.Fatalf("concurrent Redis publishers produced mappings=%d err=%v", mappings, err)
	}
	if _, err := appender.Append(ctx, stream, redisEnvelopeField, dispatch.OutboxID, []byte("different-exact-envelope")); !errors.Is(err, ErrControlDeliveryConflict) {
		t.Fatalf("stable delivery accepted conflicting exact bytes: %v", err)
	}
	if err := client.Del(ctx, stream).Err(); err != nil {
		t.Fatalf("inject hash-only Redis state: %v", err)
	}
	if err := producer.AppendPrepared(ctx, dispatch.OutboxID, prepared.Clone()); !errors.Is(err, ErrControlDeliveryIndexInconsistent) {
		t.Fatalf("hash-only Redis state did not fail closed: %v", err)
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 0 {
		t.Fatalf("hash-only rejection mutated stream: length=%d err=%v", length, err)
	}
	if mappings, err := client.HLen(ctx, deliveryIndexKey(stream)).Result(); err != nil || mappings != 1 {
		t.Fatalf("hash-only rejection mutated mappings=%d err=%v", mappings, err)
	}
	if err := client.Del(ctx, stream, deliveryIndexKey(stream)).Err(); err != nil {
		t.Fatalf("clear coordinated Redis key pair: %v", err)
	}
	if err := producer.AppendPrepared(ctx, dispatch.OutboxID, prepared.Clone()); err != nil {
		t.Fatalf("rebuild coordinated Redis key loss: %v", err)
	}
	if length, err := client.XLen(ctx, stream).Result(); err != nil || length != 1 {
		t.Fatalf("coordinated key loss was not rebuilt: length=%d err=%v", length, err)
	}
}

func TestRedisStreamServiceBackedIndexV2CutoverState(t *testing.T) {
	address := os.Getenv(redisServiceTestAddressEnv)
	if address == "" {
		t.Skipf("set %s to run the real-Redis integration test", redisServiceTestAddressEnv)
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
	stream := fmt.Sprintf("elitea:test:index-v2-cutover:%d", time.Now().UnixNano())
	group := "elitea-indexer-worker-v1"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		if err := client.Del(cleanupCtx, stream, deliveryIndexKey(stream)).Err(); err != nil {
			t.Errorf("delete Redis cutover test state: %v", err)
		}
	}()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewIndexV2CutoverReader(client, stream, group)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.ReadIndexControlState(ctx)
	if err != nil || state.StreamEntries != 0 || state.PendingEntries != 0 || state.DeliveryMappings != 0 {
		t.Fatalf("clean service-backed state=%+v err=%v", state, err)
	}
	entryID, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]any{redisEnvelopeField: "signed-v1"},
	}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.HSet(ctx, deliveryIndexKey(stream), "outbox-v1", entryID).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "worker-v1",
		Streams:  []string{stream, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatal(err)
	}
	state, err = reader.ReadIndexControlState(ctx)
	if err != nil || state.StreamEntries != 1 || state.PendingEntries != 1 || state.DeliveryMappings != 1 {
		t.Fatalf("outstanding service-backed state=%+v err=%v", state, err)
	}
}

type redisServiceHMACSigner struct{}

func (redisServiceHMACSigner) SignWorkerCommand(_ context.Context, command []byte) (Signature, error) {
	mac := hmac.New(sha256.New, []byte("ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"))
	_, _ = mac.Write(command)
	return Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyID:   "elitea-runtime-v1-conformance-hmac",
		Value:   mac.Sum(nil),
	}, nil
}

func readNewRedisMessage(t *testing.T, ctx context.Context, client *redis.Client, stream, group, consumer string) redis.XMessage {
	t.Helper()
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    1,
		Block:    2 * time.Second,
	}).Result()
	if err != nil {
		t.Fatalf("read Redis consumer-group delivery: %v", err)
	}
	if len(streams) != 1 || streams[0].Stream != stream || len(streams[0].Messages) != 1 {
		t.Fatalf("unexpected Redis consumer-group response: %+v", streams)
	}
	return streams[0].Messages[0]
}

func assertReferenceOnlyRedisMessage(t *testing.T, message redis.XMessage, limits Limits, forbidden [][]byte) []byte {
	t.Helper()
	if len(message.Values) != 1 {
		t.Fatalf("Redis control entry has %d fields, want exactly 1: %+v", len(message.Values), message.Values)
	}
	raw, ok := message.Values[redisEnvelopeField]
	if !ok {
		t.Fatalf("Redis control entry lacks %q: %+v", redisEnvelopeField, message.Values)
	}
	var value []byte
	switch typed := raw.(type) {
	case string:
		value = []byte(typed)
	case []byte:
		value = append([]byte(nil), typed...)
	default:
		t.Fatalf("Redis control field used non-binary-safe type %T", raw)
	}
	if len(value) > limits.MaxRedisFieldBytes {
		t.Fatalf("Redis control field is %d bytes, limit %d", len(value), limits.MaxRedisFieldBytes)
	}
	if size := encodedRedisEntryBytes(redisEnvelopeField, value); size > limits.MaxRedisEntryBytes {
		t.Fatalf("complete Redis control entry is %d bytes, limit %d", size, limits.MaxRedisEntryBytes)
	}
	for _, canary := range forbidden {
		if bytes.Contains(value, canary) {
			t.Fatalf("Redis control entry contains forbidden body canary %q", canary[:min(len(canary), 64)])
		}
	}
	return value
}

func assertPendingCount(t *testing.T, ctx context.Context, client *redis.Client, stream, group string, expected int64) {
	t.Helper()
	pending, err := client.XPending(ctx, stream, group).Result()
	if err != nil {
		t.Fatalf("inspect Redis pending deliveries: %v", err)
	}
	if pending.Count != expected {
		t.Fatalf("Redis pending delivery count is %d, want %d", pending.Count, expected)
	}
}
