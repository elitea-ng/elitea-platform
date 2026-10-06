package commandbus

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRouteTokenAcceptsOnlyTheContractShape(t *testing.T) {
	for stream, want := range map[string]string{
		StreamValidate:      "validate",
		StreamAgent:         "agent",
		StreamIndex:         "index",
		"ELITEA_RT_V1_A2":   "a2",
		"ELITEA_RT_V1_ZZZZ": "zzzz",
	} {
		got, err := RouteToken(stream)
		if err != nil || got != want {
			t.Errorf("RouteToken(%q) = %q, %v; want %q", stream, got, err, want)
		}
	}
	for _, stream := range []string{
		"", "ELITEA_RT_V1_", "ELITEA_RT_V1_agent", "ELITEA_RT_V1_2AGENT", "ELITEA_RT_V1_AG.ENT",
		"ELITEA_RT_V1_AG_ENT", "ELITEA_RT_V2_AGENT", "commands.v1.agent.execute.agent.shared.1.0",
		"ELITEA_RT_V1_" + strings.Repeat("A", 33),
	} {
		if _, err := RouteToken(stream); !errors.Is(err, ErrInvalidStream) {
			t.Errorf("RouteToken(%q) accepted a name outside the contract", stream)
		}
	}
}

func TestDeliverySubjectIsTheHashOfTheDeliveryID(t *testing.T) {
	// sha256("outbox-1"), computed independently (shasum -a 256). The Rust
	// and Python workers pin the same vector.
	if got := DeliveryToken("outbox-1"); got != "ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164" {
		t.Fatalf("DeliveryToken(outbox-1) = %q", got)
	}
	subject, err := DeliverySubject(StreamAgent, "a.b *>c d")
	if err != nil {
		t.Fatal(err)
	}
	if want := "elitea.rt.v1.agent.d." + DeliveryToken("a.b *>c d"); subject != want {
		t.Fatalf("subject %q, want %q", subject, want)
	}
	if strings.Count(subject, ".") != 5 {
		t.Fatalf("a delivery ID's own dots leaked into the subject: %q", subject)
	}
	filter, err := FilterSubject(StreamAgent)
	if err != nil || filter != "elitea.rt.v1.agent.d.*" {
		t.Fatalf("FilterSubject = %q, %v", filter, err)
	}
	key, err := DeadLetterKey(StreamIndex, "x")
	if err != nil || key != "index."+DeliveryToken("x") {
		t.Fatalf("DeadLetterKey = %q, %v", key, err)
	}
}

func TestValidateRoutePairsEachStreamWithItsDurable(t *testing.T) {
	for stream, consumer := range KnownStreams {
		if err := ValidateRoute(stream, consumer); err != nil {
			t.Errorf("ValidateRoute(%s, %s): %v", stream, consumer, err)
		}
		if err := ValidateRoute(stream, ""); err != nil {
			t.Errorf("ValidateRoute(%s, \"\"): %v", stream, err)
		}
	}
	if err := ValidateRoute(StreamAgent, ConsumerIndex); err == nil {
		t.Error("the agent stream was accepted with the index durable")
	}
	if err := ValidateRoute("ELITEA_RT_V1_OTHER", ""); err == nil {
		t.Error("a stream with no permission row and no bootstrap block was accepted")
	}
}

func TestTimingInvariants(t *testing.T) {
	if InProgressInterval*4 > AckWait {
		t.Errorf("InProgressInterval %s leaves under four heartbeats per AckWait %s", InProgressInterval, AckWait)
	}
	if AckWait < 2*30_000_000_000 {
		t.Errorf("AckWait %s is under twice the 30s claim lease", AckWait)
	}
	if PoisonDelay >= 24*60*60*1_000_000_000+MaxAgeMargin {
		t.Errorf("PoisonDelay %s outlives the agent stream's MaxAge", PoisonDelay)
	}
}

// The normative document names every constant this package defines. A rename
// on one side only fails here, not in a cluster.
func TestContractDocumentNamesTheConstants(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	doc := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "docs", "runtime-command-bus.md")
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read the contract document: %v", err)
	}
	text := string(raw)
	for _, name := range []string{
		StreamValidate, StreamAgent, StreamIndex,
		ConsumerValidate, ConsumerAgent, ConsumerIndex,
		HeaderMsgID, HeaderDeliveryID, DeadLetterBucket, ReplayWakeSubject,
		"elitea.rt.v1.<route>.d.<sha256(delivery_id)>",
		"max_transport_payload_bytes", "max_transport_message_bytes",
		"elitea.runtime.limits.conformance.v3",
	} {
		if !strings.Contains(text, name) {
			t.Errorf("docs/runtime-command-bus.md does not name %q", name)
		}
	}
}
