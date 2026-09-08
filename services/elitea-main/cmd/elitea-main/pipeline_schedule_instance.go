package main

import (
	"os"
	"strings"
)

// defaultPipelineScheduleInstanceID names this replica when nothing better is
// available. It is a constant, so two replicas that both fall back to it share
// one identity — acceptable, because the occurrence lease is held in the
// database and a shared name only costs a retry, never a double run.
const defaultPipelineScheduleInstanceID = "elitea-main"

// pipelineScheduleInstanceID gives the pipeline schedule runner a replica name
// that the scheduler will accept.
//
// It exists because the pipeline schedule job took its instance name from
// ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID, and runtimecomposition only reads that
// variable when ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED is "true" — it REFUSES
// the variable otherwise (internal/runtimecomposition/config.go). No stack in
// deploy/ turns index scheduling on, so the field was always empty there, the
// scheduler rejected the empty name, and the fail-fast at the composition root
// stopped elitea-main with
//
//	compose pipeline schedule job: construct pipeline schedule runner:
//	invalid scheduler configuration: instance ID must be a bounded canonical
//	identifier
//
// which took down every stack-backed CI journey at boot. The two features are
// unrelated: unattended pipelines must run on a deployment that indexes nothing
// on a schedule. So this derives a name of its own instead of borrowing one.
//
// The host name is the preferred source: in a container it is the container id
// and in Kubernetes it is the pod name, so each replica gets a DISTINCT lease
// holder, which is what a cross-replica lease wants. An operator-set value
// still wins, so a deployment that names its scheduler keeps one name for both
// runners.
func pipelineScheduleInstanceID(configured string) string {
	if identifier := canonicalScheduleInstanceID(configured); identifier != "" {
		return identifier
	}
	if hostname, err := os.Hostname(); err == nil {
		if identifier := canonicalScheduleInstanceID(hostname); identifier != "" {
			return identifier
		}
	}
	return defaultPipelineScheduleInstanceID
}

// canonicalScheduleInstanceID projects arbitrary text onto the scheduler's
// identifier alphabet, or answers "" when nothing usable survives.
//
// The scheduler accepts `^[a-z0-9][a-z0-9._-]{0,127}$`
// (internal/application/scheduling/types.go), so this lower-cases, drops every
// other byte, refuses to open with a separator, and bounds the length. It never
// SUBSTITUTES a character: a name that shares no bytes with the alphabet is
// reported as absent rather than silently turned into something else.
func canonicalScheduleInstanceID(raw string) string {
	var builder strings.Builder
	for index := range len(raw) {
		if builder.Len() >= 128 {
			break
		}
		character := raw[index]
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		switch {
		case (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9'):
			builder.WriteByte(character)
		case character == '.' || character == '_' || character == '-':
			// Never first: the pattern demands an alphanumeric opener.
			if builder.Len() > 0 {
				builder.WriteByte(character)
			}
		}
	}
	return builder.String()
}
