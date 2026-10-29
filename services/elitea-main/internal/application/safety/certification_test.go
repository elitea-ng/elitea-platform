package safety

import "testing"

// TestCertificationSuspensionAnswersItsOwnCode: a suspension the
// recertification routine wrote (ADR-0040 decision 5) refuses with
// agent_certification_lapsed; every other source keeps execution_suspended.
func TestCertificationSuspensionAnswersItsOwnCode(t *testing.T) {
	target := Target{Scope: ScopeAgent, ProjectID: 1, ApplicationID: 31}
	lapsed := CheckSnapshot([]Suspension{{ID: 4, Target: target, Source: SourceCertification}},
		Subject{ProjectID: 1, ApplicationID: 31})
	suspended, ok := AsSuspended(lapsed)
	if !ok || suspended.Code() != CertificationLapsedCode || suspended.Body()["code"] != CertificationLapsedCode {
		t.Fatalf("certification suspension = %v", lapsed)
	}
	manual := CheckSnapshot([]Suspension{{ID: 5, Target: target, Source: SourceManual}},
		Subject{ProjectID: 1, ApplicationID: 31})
	if suspended, ok := AsSuspended(manual); !ok || suspended.Code() != ErrorCode {
		t.Fatalf("manual suspension = %v", manual)
	}
}
