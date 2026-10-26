package workspacehandoff

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// The cloud executor's application methods. The later gRPC adapter
// (workspaceruntime.Store, TurnResolver, PolicyEvaluator) calls these after
// its transport verified the claim; every method is bound to the running
// turn's execution id and epoch.

// ResolveRunningTurn answers the handoff of the thread whose running cloud
// turn is executionID; false when the execution is not one.
func (s *Service) ResolveRunningTurn(ctx context.Context, executionID string) (Handoff, bool, error) {
	if executionID == "" || len(executionID) > 64 {
		return Handoff{}, false, ErrInvalid
	}
	handoff, ok, err := s.repo.ReadByRunningExecution(ctx, executionID)
	if err != nil || !ok {
		return Handoff{}, false, err
	}
	if handoff.State != workspacestore.StateCloudRunning {
		return Handoff{}, false, nil
	}
	return handoff, true, nil
}

// The audit events only the executor observes (decision 12).
const (
	EventScanBlocked        = "handoff.scan_blocked"
	EventPullRequestOpened  = "pull_request.opened"
	EventBranchPushed       = "branch.pushed"
	maxExecutorEventAttrs   = 16
	maxExecutorEventValueRn = 200
)

// executorEventKeys is the attribute allowlist per event, with the shape each
// value must have. A scan finding's PATH is never recorded (decision 12): the
// scan event carries a count and rule ids only, and a value of the wrong
// shape (a path in `rules`, text in a count) is dropped, not recorded.
var executorEventKeys = map[string]map[string]func(string) bool{
	EventScanBlocked: {"findings": isCount, "rules": isRuleList},
	EventPullRequestOpened: {
		"toolkit": isPrintable, "branch": isPrintable, "base": isPrintable, "number": isCount,
		"url": isPrintable, "files": isCount,
	},
	EventBranchPushed: {
		"toolkit": isPrintable, "branch": isPrintable, "base": isPrintable, "url": isPrintable, "commits": isCount,
	},
}

var (
	countValue = regexp.MustCompile(`^[0-9]{1,9}$`)
	// A rule id has no dot, slash or space, so a file name cannot pass.
	ruleIDValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func isCount(value string) bool  { return countValue.MatchString(value) }
func isRuleID(value string) bool { return ruleIDValue.MatchString(value) }

// isRuleList: comma-separated rule ids, at most 32.
func isRuleList(value string) bool {
	ids := strings.Split(value, ",")
	if len(ids) > 32 {
		return false
	}
	for _, id := range ids {
		if !isRuleID(id) {
			return false
		}
	}
	return true
}

func isPrintable(value string) bool { return value != "" && utf8.ValidString(value) }

// RecordExecutorEvent writes an audit event the worker observed in the
// running turn: handoff.scan_blocked, pull_request.opened or branch.pushed.
// Only allowlisted
// attributes are recorded, bounded; anything else is dropped.
func (s *Service) RecordExecutorEvent(
	ctx context.Context, thread Thread, epoch int64, executionID, kind string, attrs map[string]string,
) error {
	keys, known := executorEventKeys[kind]
	if !known || !validThread(thread) || epoch <= 0 || executionID == "" {
		return ErrInvalid
	}
	handoff, ok, err := s.repo.Read(ctx, thread)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if handoff.State != workspacestore.StateCloudRunning || handoff.Epoch != epoch || handoff.RunningExecutionID != executionID {
		return ErrStaleEpoch
	}
	names := make([]string, 0, len(attrs))
	for name, value := range attrs {
		if valid, allowed := keys[name]; allowed && valid(value) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > maxExecutorEventAttrs {
		names = names[:maxExecutorEventAttrs]
	}
	parts := []string{fmt.Sprintf("epoch %d", epoch)}
	for _, name := range names {
		value := strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, attrs[name])
		if utf8.RuneCountInString(value) > maxExecutorEventValueRn {
			value = string([]rune(value)[:maxExecutorEventValueRn]) + "…"
		}
		parts = append(parts, name+" "+value)
	}
	s.record(ctx, thread, handoff.OwnerUserID, "", "", s.now(), kind+": "+strings.Join(parts, ", "), true)
	return nil
}

// EffectiveCloudPolicy is the policy a cloud turn claim runs under, recomputed
// now (decision 11), with the chart's sandbox resources the PolicyService
// carries.
func (s *Service) EffectiveCloudPolicy(ctx context.Context, projectID int64) (Policy, error) {
	return s.EffectivePolicy(ctx, projectID)
}
