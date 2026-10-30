// Package runcaps names the run caps of ADR-0041 decision 3 (#1496, #1498)
// and the typed outcome of a run that reached one: "Reaching a cap ends the
// run with cap_reached and the cap's name. The run is not silently
// truncated."
//
// Two places stop a run at a cap. The Rust worker counts tool calls, model
// turns and records read and ends the run with a runtime failure whose
// registered message names the cap (code RESOURCE_EXHAUSTED on the wire);
// Main maps that message to the public code CAP_REACHED. Main itself
// cancels a run past the whole-run wall cap (at lease renewal) or past its
// spend cap (a routine over the gateway's request log); the cancellation
// carries cancel_reason cap_reached and the cap's name (shared 0201, 0212).
package runcaps

// Code is the public failure code of a run that reached a cap.
const Code = "CAP_REACHED"

// The caps.
const (
	MaxWallSeconds = "max_wall_seconds"
	MaxToolCalls   = "max_tool_calls"
	MaxModelTurns  = "max_model_turns"
	MaxSpend       = "max_spend"
	MaxRecordsRead = "max_records_read"
)

// All lists every cap.
var All = []string{MaxWallSeconds, MaxToolCalls, MaxModelTurns, MaxSpend, MaxRecordsRead}

// Valid reports whether name is a cap.
func Valid(name string) bool {
	for _, cap := range All {
		if cap == name {
			return true
		}
	}
	return false
}

// The registered messages. The worker sends the three worker-side ones
// byte for byte (services/elitea-worker-rust/src/protocol/output.rs); each
// is at most 256 bytes, the runtime failure bound.
const (
	messageWall = "The run stopped because it reached the maximum run time an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
	messageToolCalls = "The run stopped because it reached the maximum number of tool calls an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
	messageModelTurns = "The run stopped because it reached the maximum number of model turns an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
	messageSpend = "The run stopped because it reached the maximum spend an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
	messageRecords = "The run stopped because it reached the maximum number of records read an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
)

// Message is the registered message of a cap; "" for an unknown name.
func Message(cap string) string {
	switch cap {
	case MaxWallSeconds:
		return messageWall
	case MaxToolCalls:
		return messageToolCalls
	case MaxModelTurns:
		return messageModelTurns
	case MaxSpend:
		return messageSpend
	case MaxRecordsRead:
		return messageRecords
	default:
		return ""
	}
}

// WorkerCap answers the cap a worker's registered message names: one of
// the three caps the worker counts. ok is false for any other message.
func WorkerCap(message string) (string, bool) {
	switch message {
	case messageToolCalls:
		return MaxToolCalls, true
	case messageModelTurns:
		return MaxModelTurns, true
	case messageRecords:
		return MaxRecordsRead, true
	default:
		return "", false
	}
}
