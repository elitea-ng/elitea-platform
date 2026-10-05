package pipelinetriggers

// What a trigger ADMITS, beside how it is authenticated (authmode.go).
//
// Legacy issue 6656 let the inbound trigger start an ordinary AGENT. An agent
// reads the sender's payload, spends model tokens and drives the version's
// toolkits under the creator's identity. Three stored facts (0142) bound
// that, and this file is their vocabulary, their create-time parse and the
// inbound checks:
//
//   - TARGET KIND. The kind of version the credential was issued for. The
//     inbound path refuses a row whose version has since changed kind
//     (run.go), so editing a pipeline into an agent does not turn its old
//     credential into an agent credential.
//   - EVENT FILTER. The provider events the trigger admits. A GitHub `ping`
//     is never a run. A new agent trigger with a provider preset admits a
//     short default list, so a star, a fork or a comment on a public
//     repository does not start a paid model call.
//   - VARIABLE OVERRIDES. Whether the body's `variables` may re-value the
//     agent's declared variables. They are substituted into the
//     instructions, so the default is off.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The stored `target_kind` vocabulary (0142's CHECK).
const (
	TargetKindPipeline = "pipeline"
	TargetKindAgent    = "agent"
)

// The provider event headers. Neither is signed by its provider: the event
// filter is a cost and noise control, never an authentication step. The
// signature (or the bearer secret) is checked before it.
const (
	GitHubEventHeader = "X-GitHub-Event"
	GitLabEventHeader = "X-Gitlab-Event"
)

// gitHubPingEvent is the delivery GitHub sends when a webhook is saved. It
// carries the hook's configuration and no event, so it never starts a run.
const gitHubPingEvent = "ping"

// everyEvent is the `events` value that clears the filter.
const everyEvent = "*"

// maxEventFilter and maxEventName bound a caller-written filter.
const (
	maxEventFilter = 32
	maxEventName   = 64
)

// defaultAgentEvents is what a new AGENT trigger with a provider preset
// admits when its writer names no events: code changes, not the activity of
// any visitor to a public repository.
var defaultAgentEvents = map[string][]string{
	ProviderGitHub: {"push", "pull_request"},
	ProviderGitLab: {"Push Hook", "Merge Request Hook"},
}

// errInvalidTriggerControls is the create-time refusal for a malformed
// `events` or `allow_variable_overrides`.
var errInvalidTriggerControls = errors.New("pipelinetriggers: invalid trigger controls")

// triggerControls are the three 0142 facts, together.
type triggerControls struct {
	TargetKind             string
	EventFilter            []string
	AllowVariableOverrides bool
}

// controlsBody is the part of the create/rotate body this file reads. Both
// fields are pointers so "absent" and "named" stay two different answers.
type controlsBody struct {
	Events                 *json.RawMessage `json:"events"`
	AllowVariableOverrides *bool            `json:"allow_variable_overrides"`
}

// requestedControls is what the body asked for.
type requestedControls struct {
	eventsNamed    bool
	events         []string
	overridesNamed bool
	overrides      bool
}

// parseTriggerControls reads `events` and `allow_variable_overrides` out of
// the create/rotate body. An unreadable body asks for nothing, the same rule
// parseAuthMode applies.
//
// `events` is a list of event names, or `["*"]` for every event. An empty
// list is refused: a trigger that should admit nothing is revoked instead.
func parseTriggerControls(raw []byte) (requestedControls, error) {
	var requested requestedControls
	if len(raw) == 0 {
		return requested, nil
	}
	var body controlsBody
	if err := json.Unmarshal(raw, &body); err != nil {
		// `{"allow_variable_overrides":"yes"}` is a readable body with a
		// wrong type, not an unrelated body. Refused, rather than read as
		// "nothing asked".
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil {
			if _, ok := probe["allow_variable_overrides"]; ok {
				return requested, errInvalidTriggerControls
			}
			if _, ok := probe["events"]; ok {
				return requested, errInvalidTriggerControls
			}
		}
		return requested, nil
	}
	if body.AllowVariableOverrides != nil {
		requested.overridesNamed = true
		requested.overrides = *body.AllowVariableOverrides
	}
	if body.Events != nil && string(*body.Events) != "null" {
		var events []string
		if err := json.Unmarshal(*body.Events, &events); err != nil {
			return requested, errInvalidTriggerControls
		}
		filter, err := normalizeEventFilter(events)
		if err != nil {
			return requested, err
		}
		requested.eventsNamed = true
		requested.events = filter
	}
	return requested, nil
}

// normalizeEventFilter validates a written list. `["*"]` is nil (every event).
func normalizeEventFilter(events []string) ([]string, error) {
	if len(events) == 0 || len(events) > maxEventFilter {
		return nil, errInvalidTriggerControls
	}
	if len(events) == 1 && strings.TrimSpace(events[0]) == everyEvent {
		return nil, nil
	}
	filter := make([]string, 0, len(events))
	seen := map[string]bool{}
	for _, event := range events {
		name := strings.TrimSpace(event)
		if !validEventName(name) {
			return nil, errInvalidTriggerControls
		}
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		filter = append(filter, name)
	}
	return filter, nil
}

// validEventName admits the characters GitHub and GitLab event names use:
// GitHub's `pull_request`, GitLab's `Merge Request Hook`.
func validEventName(name string) bool {
	if name == "" || len(name) > maxEventName {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-', character == '.', character == ' ':
		default:
			return false
		}
	}
	return true
}

// targetKindOf names the kind of a resolved version.
func targetKindOf(target runTarget) string {
	if target.IsPipeline {
		return TargetKindPipeline
	}
	return TargetKindAgent
}

// resolveControls decides the three facts a create or a rotation stores.
//
//   - The kind is ALWAYS the version's current kind: minting is an explicit
//     issue for what the version is now.
//   - The event filter is the body's when it names one. Otherwise a rotation
//     that keeps the stored mode, for a version of the same kind, keeps the
//     stored filter. Anything else (a first create, a new mode, a new kind)
//     takes the default: a short list for an agent with a provider preset,
//     and every event for everything else.
//   - The variable opt-in is the body's when it names one, else the stored
//     one, else off.
//
// A filter on a CUSTOM trigger is refused: a custom sender sends no provider
// event header, so the filter would refuse every call.
func resolveControls(
	requested requestedControls, mode triggerAuthMode, modeNamed bool,
	previous triggerRow, hasPrevious bool, target runTarget,
) (triggerControls, error) {
	controls := triggerControls{TargetKind: targetKindOf(target)}

	keepStored := hasPrevious && !modeNamed && previous.TargetKind == controls.TargetKind
	switch {
	case requested.eventsNamed:
		controls.EventFilter = requested.events
	case keepStored:
		controls.EventFilter = previous.EventFilter
	case controls.TargetKind == TargetKindAgent:
		controls.EventFilter = defaultAgentEvents[mode.Provider]
	}
	if len(controls.EventFilter) > 0 && providerEventHeader(mode.Provider) == "" {
		return triggerControls{}, errInvalidTriggerControls
	}

	switch {
	case requested.overridesNamed:
		controls.AllowVariableOverrides = requested.overrides
	case hasPrevious:
		controls.AllowVariableOverrides = previous.AllowVariableOverrides
	}
	return controls, nil
}

// providerEventHeader is the header a provider names its event in, or "" for
// a sender that names none.
func providerEventHeader(provider string) string {
	switch provider {
	case ProviderGitHub:
		return GitHubEventHeader
	case ProviderGitLab:
		return GitLabEventHeader
	}
	return ""
}

// deliveryEvent is the provider event this request names, or "".
func deliveryEvent(trigger triggerRow, headers http.Header) string {
	header := providerEventHeader(trigger.Provider)
	if header == "" {
		return ""
	}
	return strings.TrimSpace(headers.Get(header))
}

// ignoredEventReason says why a delivery admits no run, or "" when it does.
//
// A GitHub `ping` is never a run, whatever the filter says. Otherwise a
// stored filter admits only the events it lists; an absent event header is
// not on any list.
func ignoredEventReason(trigger triggerRow, event string) string {
	if trigger.Provider == ProviderGitHub && strings.EqualFold(event, gitHubPingEvent) {
		return "a GitHub ping delivery starts no run"
	}
	if trigger.EventFilter == nil {
		return ""
	}
	for _, admitted := range trigger.EventFilter {
		if strings.EqualFold(admitted, event) {
			return ""
		}
	}
	if event == "" {
		return "the delivery names no event, and this trigger admits only listed events"
	}
	return "the event is not one this trigger admits"
}
