package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/events"
)

// A conversation activity frame goes onto the conversations family with its
// audience for the SSE relay to filter on.
func TestConversationActivityFramesCarryTheirAudience(t *testing.T) {
	if newConversationActivity(nil, nil, nil) != nil {
		t.Fatal("no bus must publish nothing")
	}
	bus := &capturedBus{}
	activity := conversationActivityBus{publisher: events.NewConversationsPublisher(bus)}
	unread := true
	if err := activity.Publish(context.Background(), 7, conversationactivity.Audience{Project: true},
		conversationactivity.Frame{ConversationID: 3, State: "idle", Since: "2026-10-20T00:00:00Z",
			LastActivityAt: "2026-10-20T00:00:00Z", UnreadHint: &unread}); err != nil {
		t.Fatal(err)
	}
	if bus.channel != "project:7:conversations" || bus.eventType != conversationactivity.EventType {
		t.Fatalf("published on %q as %q", bus.channel, bus.eventType)
	}
	var fields map[string]any
	if err := json.Unmarshal(bus.payload, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["audience_project"] != true || fields["audience"] != nil || fields["state"] != "idle" ||
		fields["unread_hint"] != true || fields["conversation_id"] != float64(3) {
		t.Fatalf("payload = %s", bus.payload)
	}

	read := false
	if err := activity.Publish(context.Background(), 7, conversationactivity.Audience{Users: []int64{11}},
		conversationactivity.Frame{ConversationID: 3, UnreadHint: &read, ReadAt: "2026-10-20T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(bus.payload, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["audience_project"] != nil || fmt.Sprint(fields["audience"]) != "[11]" || fields["unread_hint"] != false || fields["state"] != nil {
		t.Fatalf("read payload = %s", bus.payload)
	}
}
