package handoffcontinue

// Who answers the cloud turn of a work thread (ADR-0050 decisions 7c/7d,
// issue 3053).
//
// A send's continue (StartContinue) and a schedule's or a trigger's turn
// (StartThreadTurn) post a message on the handed-off thread that no person
// addresses. In a thread of two or more agents it is routed by the
// conversation's floor with the rule every other start uses
// (turntaking.RouteMessage): under `lead` the lead answers; under
// `addressed` the participant the thread's last local turn addressed (the
// sticky target) answers. The chat start (agentexecution.planFloor) does not
// route these starts itself, because they name their participant, so the
// participant is chosen here, before the start.

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"

// ThreadCandidate is one participant of a work thread that a cloud turn may
// address: an agent (application) or the model (dummy).
type ThreadCandidate struct {
	ParticipantID int64
	Agent         bool
	// VersionID is an agent participant's mapped version; 0 for the model,
	// or an agent mapped without one.
	VersionID int64
}

// RouteThread picks the participant the next cloud turn of a work thread
// addresses. candidates are the thread's agent and model participants in
// participant order; sticky is the participant the thread's last local turn
// addressed (0 for none). false: the thread has no candidate.
//
// With two or more agents the floor decides (turntaking.RouteMessage, no
// address). A floor that cannot route (`addressed` with no sticky agent)
// keeps the order a thread of one agent uses: the sticky participant, else
// the first agent, else the model.
func RouteThread(settings turntaking.Settings, sticky int64, candidates []ThreadCandidate) (ThreadCandidate, bool) {
	agents := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Agent {
			agents = append(agents, candidate.ParticipantID)
		}
	}
	if len(agents) >= 2 {
		if targets, err := turntaking.RouteMessage(settings, sticky, nil, agents); err == nil && len(targets) > 0 {
			if routed, ok := findCandidate(candidates, targets[0]); ok {
				return routed, true
			}
		}
	}
	if sticky > 0 {
		if found, ok := findCandidate(candidates, sticky); ok {
			return found, true
		}
	}
	for _, candidate := range candidates {
		if candidate.Agent {
			return candidate, true
		}
	}
	if len(candidates) > 0 {
		return candidates[0], true
	}
	return ThreadCandidate{}, false
}

func findCandidate(candidates []ThreadCandidate, id int64) (ThreadCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.ParticipantID == id {
			return candidate, true
		}
	}
	return ThreadCandidate{}, false
}
