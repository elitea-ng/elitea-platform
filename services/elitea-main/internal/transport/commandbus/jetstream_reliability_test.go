package commandbus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

const natsReliabilityRoundsEnv = "ELITEA_RUNTIME_NATS_RELIABILITY_ROUNDS"

// TestJetStreamCapacityReliability is an opt-in load test of the command bus's
// server-enforced invariants, on the chart's own permissions: many publishers
// on several connections race distinct and repeated deliveries into a small
// stream. Every round proves the live set never exceeds MaxMsgs, a delivery is
// never live twice, every refusal is backpressure (never loss: the refused
// deliveries publish once capacity is freed), and only an ack frees capacity.
// It is not a server-outage or cluster test.
func TestJetStreamCapacityReliability(t *testing.T) {
	raw := os.Getenv(natsReliabilityRoundsEnv)
	if raw == "" {
		t.Skipf("set %s (1..100) to run the JetStream capacity reliability test", natsReliabilityRoundsEnv)
	}
	rounds, err := strconv.Atoi(raw)
	if err != nil || rounds < 1 || rounds > 100 || strconv.Itoa(rounds) != raw {
		t.Fatalf("%s must be a canonical integer from 1 to 100", natsReliabilityRoundsEnv)
	}
	const (
		capacity   = 16
		deliveries = 64
		publishers = 4
		attempts   = 3 // each delivery is offered this many times per round
	)
	s := natstest.Start(t)
	s.Bootstrap(t, map[string]string{"NATS_RT_INDEX_MAX_MSGS": strconv.Itoa(capacity)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	appenders := make([]*JetStreamAppender, publishers)
	var observer jetstream.Stream
	for i := range appenders {
		js := securedJetStream(t, s, natsconn.IdentityMainRuntime)
		handle, err := BindStream(ctx, js, StreamRequirement{Stream: StreamIndex, MinMaxAge: 24*time.Hour + MaxAgeMargin, MaxMessageBytes: MaxMessageBytes})
		if err != nil {
			t.Fatal(err)
		}
		observer = handle
		if appenders[i], err = NewJetStreamAppender(js, handle); err != nil {
			t.Fatal(err)
		}
	}
	consumer, err := securedJetStream(t, s, natsconn.IdentityWorker).Consumer(ctx, StreamIndex, ConsumerIndex)
	if err != nil {
		t.Fatal(err)
	}

	for round := range rounds {
		pending := make(map[string][]byte, deliveries)
		for d := range deliveries {
			pending[fmt.Sprintf("round-%d-delivery-%d", round, d)] = []byte(fmt.Sprintf("envelope-%d-%d", round, d))
		}
		for len(pending) > 0 {
			var mu sync.Mutex
			stored := map[string]bool{}
			var wg sync.WaitGroup
			ids := make([]string, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			for a := 0; a < attempts; a++ {
				for i, id := range ids {
					wg.Add(1)
					go func(appender *JetStreamAppender, id string, value []byte) {
						defer wg.Done()
						_, err := appender.Append(ctx, StreamIndex, id, value)
						var saturated *ControlStreamSaturatedError
						switch {
						case err == nil:
							mu.Lock()
							stored[id] = true
							mu.Unlock()
						case errors.As(err, &saturated):
						default:
							t.Errorf("append %s: %v", id, err)
						}
					}(appenders[(i+a)%publishers], id, pending[id])
				}
			}
			wg.Wait()
			info, err := observer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.State.Msgs > capacity || info.State.NumSubjects != info.State.Msgs {
				t.Fatalf("round %d: live=%d subjects=%d capacity=%d", round, info.State.Msgs, info.State.NumSubjects, capacity)
			}
			if uint64(len(stored)) != info.State.Msgs {
				t.Fatalf("round %d: %d deliveries reported stored, %d live", round, len(stored), info.State.Msgs)
			}
			// Settle everything that is live; only then is there room again.
			batch, err := consumer.Fetch(int(info.State.Msgs), jetstream.FetchMaxWait(5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			acked := 0
			for msg := range batch.Messages() {
				id := msg.Headers().Get(HeaderDeliveryID)
				if string(msg.Data()) != string(pending[id]) {
					t.Fatalf("round %d: delivery %s carries other bytes", round, id)
				}
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
				delete(pending, id)
				acked++
			}
			if acked != len(stored) {
				t.Fatalf("round %d: acked %d of %d live deliveries", round, acked, len(stored))
			}
		}
	}
	s.RequireNoViolations(t, natsconn.IdentityMainRuntime)
	s.RequireNoViolations(t, natsconn.IdentityWorker)
}
