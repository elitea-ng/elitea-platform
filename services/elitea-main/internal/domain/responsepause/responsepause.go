// Package responsepause names when a chat response waits for a person's
// decision: a root HITL pause, a delegated sign-in request, or a parked
// fan-out (a Parallel or Map whose members wait on cards, contract §8). Every
// reader that asks "is this run waiting for someone?" uses this one set, so a
// new pause kind cannot be missed by one of them (FU-284).
package responsepause

import (
	"regexp"
	"strings"
)

// WaitingKeys are the chat_message_group.meta members of a response that
// waits for a person's decision.
var WaitingKeys = []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "fanout_parked_v1"}

// Alias is the table alias a caller reads chat_message_group under. Only the
// values below exist, so no caller data reaches the SQL text.
type Alias struct{ name string }

// The aliases the readers use.
var (
	// Unqualified reads the row's own meta (a single-table query).
	Unqualified = Alias{}
	// Response is the editor test runs' response row.
	Response = Alias{name: "response"}
	// Runs is the workspace hand-back's run row.
	Runs = Alias{name: "runs"}
	// Group is the chat activity reader's message group.
	Group = Alias{name: "g"}
)

// identifier is the backstop: an alias is a plain lower-case identifier.
var identifier = regexp.MustCompile(`^[a-z_]{1,32}$`)

// SQL is a boolean SQL expression: the response under alias waits for a
// person. It is built only from the constants above.
func SQL(alias Alias) string {
	column := "meta"
	if alias.name != "" {
		if !identifier.MatchString(alias.name) {
			return "FALSE"
		}
		column = alias.name + ".meta"
	}
	quoted := make([]string, len(WaitingKeys))
	for i, key := range WaitingKeys {
		quoted[i] = "'" + key + "'"
	}
	return "(" + column + " ?| ARRAY[" + strings.Join(quoted, ", ") + "]::text[])"
}

// Waiting reports whether a response's meta keys hold a wait for a person.
func Waiting(meta map[string]any) bool {
	for _, key := range WaitingKeys {
		if _, ok := meta[key]; ok {
			return true
		}
	}
	return false
}
