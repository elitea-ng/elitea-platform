package budgetwriteback

import (
	"context"
	"os"
	"testing"
	"time"
)

// Compose's plaintext posture (one global account, no client identity) keeps
// the default $JS.API prefix: the deltas stream is in the scheduler's own
// account there. Only a client identity — the chart's accounts — switches the
// handle to the SCHEDULER account's import prefix (#1076). AccountInfo
// answers only on the default prefix of a plain JetStream server, so it tells
// the two apart.
func TestPlaintextJetStreamKeepsTheDefaultAPIPrefix(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_NATS_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_NATS_URL (a JetStream-enabled plaintext NATS) to run the compose-posture test")
	}
	conn := &Connector{Config: DialConfig{URL: url}}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream on a plaintext URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := js.AccountInfo(ctx); err != nil {
		t.Fatalf("AccountInfo on the plaintext posture = %v; the handle must use the default $JS.API prefix", err)
	}
}
