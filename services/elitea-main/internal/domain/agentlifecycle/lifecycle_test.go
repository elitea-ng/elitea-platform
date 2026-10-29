package agentlifecycle

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	refusal, ok := AsRefusal(err)
	if !ok {
		t.Fatalf("not a typed refusal: %v", err)
	}
	return refusal.Code
}

func TestDefaultPolicyIsTheADRTable(t *testing.T) {
	policy := DefaultPolicy()
	cases := []struct {
		tier                  Tier
		approvals, governance int
		cadence               time.Duration
		attestations          int
	}{
		{TierLow, 0, 0, 0, 0},
		{TierMedium, 1, 0, 365 * Day, 0},
		{TierHigh, 1, 1, 180 * Day, 0},
		{TierCritical, 2, 1, 90 * Day, 2},
	}
	for _, c := range cases {
		got := policy.Requirement(c.tier)
		if got.Approvals != c.approvals || got.GovernanceApprovals != c.governance ||
			got.RecertifyEvery != c.cadence || len(got.Attestations) != c.attestations {
			t.Errorf("%s: %+v", c.tier, got)
		}
	}
	if policy.LapseAction != LapseWarn {
		t.Errorf("lapse action = %s", policy.LapseAction)
	}
	if !policy.Requirement(TierMedium).RequiresApproval() || policy.Requirement(TierLow).RequiresApproval() {
		t.Error("only low publishes without approval")
	}
}

func TestPolicyNeverGoesBelowTheFloor(t *testing.T) {
	policy := Policy{Tiers: map[Tier]Requirement{
		TierMedium:   {Approvals: 0},
		TierHigh:     {Approvals: 1, GovernanceApprovals: 0},
		TierCritical: {Approvals: 1, GovernanceApprovals: 3, RecertifyEvery: -Day},
		TierLow:      {Approvals: 9, GovernanceApprovals: 1, RecertifyEvery: 5000 * Day},
	}}
	if got := policy.Requirement(TierMedium); got.Approvals != 1 {
		t.Errorf("medium lowered to %d approvals", got.Approvals)
	}
	if got := policy.Requirement(TierHigh); got.GovernanceApprovals != 1 {
		t.Errorf("high lost its governance approval: %+v", got)
	}
	critical := policy.Requirement(TierCritical)
	if critical.Approvals != 2 || critical.GovernanceApprovals != 2 || critical.RecertifyEvery != 0 {
		t.Errorf("critical = %+v", critical)
	}
	// A project or an administrator may raise a tier: low can need approvals.
	low := policy.Requirement(TierLow)
	if low.Approvals != MaxApprovals || low.GovernanceApprovals != 1 || low.RecertifyEvery != MaxRecertifyDays*Day {
		t.Errorf("low = %+v", low)
	}
	// An unknown tier is read as critical.
	if got := policy.Requirement(Tier("bogus")); got.Approvals != 2 {
		t.Errorf("unknown tier = %+v", got)
	}
}

func TestEvaluateApprovalMakerChecker(t *testing.T) {
	medium := DefaultPolicy().Requirement(TierMedium)
	high := DefaultPolicy().Requirement(TierHigh)
	critical := DefaultPolicy().Requirement(TierCritical)

	cases := []struct {
		name     string
		request  ApprovalRequest
		complete bool
		code     string
	}{
		{"medium self approval", ApprovalRequest{Requirement: medium, ProposerID: 7, ApproverID: 7}, false, SelfApprovalCode},
		{"medium approver", ApprovalRequest{Requirement: medium, ProposerID: 7, ApproverID: 8}, true, ""},
		{"no signed-in approver", ApprovalRequest{Requirement: medium, ProposerID: 7}, false, SelfApprovalCode},
		{"high without governance", ApprovalRequest{Requirement: high, ProposerID: 7, ApproverID: 8}, false, ApproverRoleCode},
		{"high governance self", ApprovalRequest{Requirement: high, ProposerID: 7, ApproverID: 7, ApproverGovernance: true}, false, SelfApprovalCode},
		{"high with governance", ApprovalRequest{Requirement: high, ProposerID: 7, ApproverID: 8, ApproverGovernance: true}, true, ""},
		{"critical first plain approver", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 8}, false, ""},
		{"critical first governance approver", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 8, ApproverGovernance: true}, false, ""},
		{"critical second plain after plain", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 9,
			Existing: []Approval{{ApproverID: 8}}}, false, ApproverRoleCode},
		{"critical second governance after plain", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 9,
			ApproverGovernance: true, Existing: []Approval{{ApproverID: 8}}}, true, ""},
		{"critical second plain after governance", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 9,
			Existing: []Approval{{ApproverID: 8, Governance: true}}}, true, ""},
		{"critical same approver twice", ApprovalRequest{Requirement: critical, ProposerID: 7, ApproverID: 8,
			ApproverGovernance: true, Existing: []Approval{{ApproverID: 8, Governance: true}}}, false, AlreadyApprovedCode},
		{"already complete", ApprovalRequest{Requirement: medium, ProposerID: 7, ApproverID: 9,
			Existing: []Approval{{ApproverID: 8}}}, false, StateConflictCode},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			complete, err := EvaluateApproval(c.request)
			if got := refusalCode(t, err); got != c.code {
				t.Fatalf("code = %q, want %q (err %v)", got, c.code, err)
			}
			if complete != c.complete {
				t.Fatalf("complete = %v, want %v", complete, c.complete)
			}
		})
	}
}

func TestChangeStateMachine(t *testing.T) {
	legal := [][2]ChangeState{
		{ChangeDraft, ChangeSubmitted}, {ChangeDraft, ChangeWithdrawn},
		{ChangeSubmitted, ChangePublished}, {ChangeSubmitted, ChangeApproved},
		{ChangeSubmitted, ChangeRejected}, {ChangeSubmitted, ChangeWithdrawn},
	}
	for _, move := range legal {
		if !CanMove(move[0], move[1]) {
			t.Errorf("%s -> %s refused", move[0], move[1])
		}
	}
	illegal := [][2]ChangeState{
		{ChangeDraft, ChangePublished}, {ChangeDraft, ChangeRejected}, {ChangeDraft, ChangeApproved},
		{ChangePublished, ChangeWithdrawn}, {ChangeRejected, ChangeSubmitted},
		{ChangeWithdrawn, ChangeSubmitted}, {ChangeApproved, ChangePublished},
		{ChangeSubmitted, ChangeDraft},
	}
	for _, move := range illegal {
		if CanMove(move[0], move[1]) {
			t.Errorf("%s -> %s allowed", move[0], move[1])
		}
	}
	if !ChangeDraft.Open() || !ChangeSubmitted.Open() || ChangePublished.Open() {
		t.Error("open states")
	}
}

func TestCheckTierChange(t *testing.T) {
	cases := []struct {
		change TierChange
		code   string
	}{
		{TierChange{From: TierLow, To: TierLow}, ""},
		{TierChange{From: TierLow, To: TierHigh, CallerIsOwner: true}, ""},
		{TierChange{From: TierLow, To: TierHigh, CallerGovernance: true}, ""},
		{TierChange{From: TierLow, To: TierHigh}, OwnerRequiredCode},
		{TierChange{From: TierHigh, To: TierLow, CallerIsOwner: true}, TierLowerCode},
		{TierChange{From: TierHigh, To: TierMedium, CallerGovernance: true}, ""},
		{TierChange{From: TierHigh, To: Tier("extreme"), CallerGovernance: true}, InvalidCode},
	}
	for _, c := range cases {
		if got := refusalCode(t, CheckTierChange(c.change)); got != c.code {
			t.Errorf("%+v: code = %q, want %q", c.change, got, c.code)
		}
	}
}

func TestCheckGate(t *testing.T) {
	if err := CheckGate(0, GateResult{State: GateFailed}, false); err != nil {
		t.Fatalf("no gate: %v", err)
	}
	if got := refusalCode(t, CheckGate(4, GateResult{State: GatePending}, false)); got != GatePendingCode {
		t.Fatalf("pending: %q", got)
	}
	err := CheckGate(4, GateResult{State: GateFailed, Failures: []string{"case refund-policy"}}, false)
	refusal, _ := AsRefusal(err)
	if refusal == nil || refusal.Code != GateFailedCode || refusal.Status() != http.StatusConflict {
		t.Fatalf("failed: %v", err)
	}
	if failures, _ := refusal.Body()["failures"].([]string); len(failures) != 1 {
		t.Fatalf("failures not in the body: %v", refusal.Body())
	}
	if err := CheckGate(4, GateResult{State: GateFailed}, true); err != nil {
		t.Fatalf("overridden: %v", err)
	}
	if err := CheckGate(4, GateResult{State: GatePassed}, false); err != nil {
		t.Fatalf("passed: %v", err)
	}
}

func TestMissingAttestations(t *testing.T) {
	required := []AttestationKind{AttestationModelRisk, AttestationLegal}
	missing := MissingAttestations(required, []AttestationKind{AttestationLegal, AttestationArchitecture})
	if len(missing) != 1 || missing[0] != AttestationModelRisk {
		t.Fatalf("missing = %v", missing)
	}
	if got := MissingAttestations(nil, nil); len(got) != 0 {
		t.Fatalf("nothing required: %v", got)
	}
}

func TestAttestationValidity(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	if !(Attestation{}).Valid(now) || !(Attestation{ValidUntil: &future}).Valid(now) {
		t.Error("a current attestation is invalid")
	}
	if (Attestation{ValidUntil: &past}).Valid(now) || (Attestation{RevokedAt: &past}).Valid(now) {
		t.Error("an expired or revoked attestation is valid")
	}
}

func TestRefusalStatuses(t *testing.T) {
	cases := map[string]int{
		ChangeRequiredCode: http.StatusConflict, SelfApprovalCode: http.StatusForbidden,
		ApproverRoleCode: http.StatusForbidden, GatePendingCode: http.StatusConflict,
		GateFailedCode: http.StatusConflict, AttestationMissingCode: http.StatusConflict,
		LapsedCode: http.StatusConflict, InvalidCode: http.StatusBadRequest, NotFoundCode: http.StatusNotFound,
		TierLowerCode: http.StatusForbidden,
	}
	for code, status := range cases {
		if got := Refuse(code, "x").Status(); got != status {
			t.Errorf("%s: %d, want %d", code, got, status)
		}
	}
	wrapped := errors.Join(errors.New("context"), Refuse(StaleCode, "x"))
	if refusal, ok := AsRefusal(wrapped); !ok || refusal.Code != StaleCode {
		t.Error("a wrapped refusal is not found")
	}
	body := RefuseWith(AttestationMissingCode, "m", map[string]any{"code": "spoof", "missing": []string{"legal"}}).Body()
	if body["code"] != AttestationMissingCode || body["missing"] == nil {
		t.Errorf("body = %v", body)
	}
}

func TestNormalizeDataClasses(t *testing.T) {
	got, err := NormalizeDataClasses([]string{" PII ", "pii", "finance.restricted", ""})
	if err != nil || len(got) != 2 || got[0] != "pii" || got[1] != "finance.restricted" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := NormalizeDataClasses([]string{"no spaces allowed"}); err == nil {
		t.Fatal("a malformed class was accepted")
	}
	many := make([]string, MaxDataClasses+1)
	for i := range many {
		many[i] = "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := NormalizeDataClasses(many); err == nil {
		t.Fatal("an excess list was accepted")
	}
}

func TestRecertifyBy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if DefaultPolicy().Requirement(TierLow).RecertifyBy(now) != nil {
		t.Error("low has a cadence")
	}
	deadline := DefaultPolicy().Requirement(TierCritical).RecertifyBy(now)
	if deadline == nil || !deadline.Equal(now.Add(90*Day)) {
		t.Errorf("critical deadline = %v", deadline)
	}
}
