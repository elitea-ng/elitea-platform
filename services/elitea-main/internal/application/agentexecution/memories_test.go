package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestAppendCurrentInstructionsMemories(t *testing.T) {
	cases := []struct {
		name         string
		instructions string
		memoryText   string
		want         string
	}{
		{"no memories is a no-op", "Be helpful.", "", "Be helpful."},
		{"no prior instructions uses memory text alone", "", "Remembers: likes Go.", "Remembers: likes Go."},
		{"both present concatenate with a blank line, memory last", "Be helpful.", "Remembers: likes Go.", "Be helpful.\n\nRemembers: likes Go."},
		{"both empty stays empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendCurrentInstructionsMemories(tc.instructions, tc.memoryText)
			if got != tc.want {
				t.Errorf("appendCurrentInstructionsMemories(%q, %q) = %q, want %q", tc.instructions, tc.memoryText, got, tc.want)
			}
		})
	}
}

func TestAppendCurrentApplicationMemories(t *testing.T) {
	t.Run("splices onto an existing instructions field without disturbing siblings", func(t *testing.T) {
		versionDetails := json.RawMessage(`{"instructions":"Be helpful.","step_limit":25,"agent_type":"agent"}`)
		got := appendCurrentApplicationMemories(versionDetails, "Remembers: likes Go.")

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		var instructions string
		if err := json.Unmarshal(fields["instructions"], &instructions); err != nil {
			t.Fatalf("decode instructions: %v", err)
		}
		if instructions != "Be helpful.\n\nRemembers: likes Go." {
			t.Errorf("instructions = %q", instructions)
		}
		// step_limit's exact encoding (an integer, not a float) must survive
		// untouched — this is the whole reason the splice decodes into
		// map[string]json.RawMessage rather than map[string]any.
		if string(fields["step_limit"]) != "25" {
			t.Errorf("step_limit re-encoded as %q, want the original 25 (not 25.0)", fields["step_limit"])
		}
	})

	t.Run("empty memory text is a no-op", func(t *testing.T) {
		versionDetails := json.RawMessage(`{"instructions":"Be helpful."}`)
		got := appendCurrentApplicationMemories(versionDetails, "")
		if string(got) != string(versionDetails) {
			t.Errorf("empty memory text changed versionDetails: %s", got)
		}
	})

	t.Run("no instructions key uses memory text alone", func(t *testing.T) {
		versionDetails := json.RawMessage(`{"agent_type":"agent"}`)
		got := appendCurrentApplicationMemories(versionDetails, "Remembers: likes Go.")
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		var instructions string
		if err := json.Unmarshal(fields["instructions"], &instructions); err != nil {
			t.Fatalf("decode instructions: %v", err)
		}
		if instructions != "Remembers: likes Go." {
			t.Errorf("instructions = %q", instructions)
		}
	})

	t.Run("malformed versionDetails degrades to unchanged, never errors", func(t *testing.T) {
		versionDetails := json.RawMessage(`not json`)
		got := appendCurrentApplicationMemories(versionDetails, "Remembers: likes Go.")
		if string(got) != string(versionDetails) {
			t.Errorf("malformed versionDetails changed: %s", got)
		}
	})

	t.Run("empty versionDetails is a no-op", func(t *testing.T) {
		got := appendCurrentApplicationMemories(nil, "Remembers: likes Go.")
		if got != nil {
			t.Errorf("nil versionDetails produced %s, want nil unchanged", got)
		}
	})
}

type stubMemoryRecallResolver struct {
	recall      CurrentMemoryRecall
	resolveErr  error
	recordErr   error
	recordCalls []struct {
		projectID         int64
		responseMessageID string
		count             int
	}
}

func (s *stubMemoryRecallResolver) ResolveCurrentMemoryRecall(_ context.Context, _, _ int64, _ string) (CurrentMemoryRecall, error) {
	if s.resolveErr != nil {
		return CurrentMemoryRecall{}, s.resolveErr
	}
	return s.recall, nil
}

func (s *stubMemoryRecallResolver) RecordCurrentMemoryUsage(_ context.Context, projectID int64, responseMessageID string, count int) error {
	s.recordCalls = append(s.recordCalls, struct {
		projectID         int64
		responseMessageID string
		count             int
	}{projectID, responseMessageID, count})
	return s.recordErr
}

func TestResolveCurrentMemoryRecallDegradesOnAbsentOrFailingResolver(t *testing.T) {
	t.Run("no resolver attached answers the zero value", func(t *testing.T) {
		service := &CurrentApplicationStartService{}
		got := service.resolveCurrentMemoryRecall(context.Background(), 1, 7, "hello")
		if got.Text != "" || got.Count != 0 || len(got.IDs) != 0 {
			t.Errorf("resolveCurrentMemoryRecall with no resolver = %+v, want zero value", got)
		}
	})

	t.Run("a failing resolver degrades to the zero value, not an error", func(t *testing.T) {
		service := &CurrentApplicationStartService{}
		service = service.WithMemories(&stubMemoryRecallResolver{resolveErr: errors.New("store unavailable")})
		got := service.resolveCurrentMemoryRecall(context.Background(), 1, 7, "hello")
		if got.Text != "" || got.Count != 0 || len(got.IDs) != 0 {
			t.Errorf("resolveCurrentMemoryRecall on a failing resolver = %+v, want zero value (fail open)", got)
		}
	})

	t.Run("a successful resolver's answer passes through unchanged", func(t *testing.T) {
		want := CurrentMemoryRecall{Text: "Remembers: likes Go.", Count: 1, IDs: []string{"9"}}
		service := &CurrentApplicationStartService{}
		service = service.WithMemories(&stubMemoryRecallResolver{recall: want})
		got := service.resolveCurrentMemoryRecall(context.Background(), 1, 7, "hello")
		if got.Text != want.Text || got.Count != want.Count {
			t.Errorf("resolveCurrentMemoryRecall = %+v, want %+v", got, want)
		}
	})
}

func TestRecordCurrentMemoryUsageIsBestEffortAndNeverCalledForZero(t *testing.T) {
	t.Run("no resolver attached is a silent no-op", func(t *testing.T) {
		service := &CurrentApplicationStartService{}
		service.recordCurrentMemoryUsage(context.Background(), 1, "resp-id", CurrentMemoryRecall{Count: 3})
		// No panic, nothing to assert beyond "did not blow up".
	})

	t.Run("zero count never calls the resolver", func(t *testing.T) {
		stub := &stubMemoryRecallResolver{}
		service := (&CurrentApplicationStartService{}).WithMemories(stub)
		service.recordCurrentMemoryUsage(context.Background(), 1, "resp-id", CurrentMemoryRecall{Count: 0})
		if len(stub.recordCalls) != 0 {
			t.Errorf("recordCurrentMemoryUsage called the resolver for a zero count: %+v", stub.recordCalls)
		}
	})

	t.Run("a positive count calls RecordCurrentMemoryUsage with the turn's own ids", func(t *testing.T) {
		stub := &stubMemoryRecallResolver{}
		service := (&CurrentApplicationStartService{}).WithMemories(stub)
		service.recordCurrentMemoryUsage(context.Background(), 42, "resp-id", CurrentMemoryRecall{Count: 3})
		if len(stub.recordCalls) != 1 {
			t.Fatalf("recordCurrentMemoryUsage called the resolver %d times, want 1", len(stub.recordCalls))
		}
		call := stub.recordCalls[0]
		if call.projectID != 42 || call.responseMessageID != "resp-id" || call.count != 3 {
			t.Errorf("recordCurrentMemoryUsage call = %+v, want {42 resp-id 3}", call)
		}
	})

	t.Run("the resolver's own error is swallowed, never propagated", func(t *testing.T) {
		stub := &stubMemoryRecallResolver{recordErr: errors.New("write failed")}
		service := (&CurrentApplicationStartService{}).WithMemories(stub)
		// Must not panic even though RecordCurrentMemoryUsage errors.
		service.recordCurrentMemoryUsage(context.Background(), 1, "resp-id", CurrentMemoryRecall{Count: 1})
	})
}
