package toolkitcalltool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The Rust capability reason, verbatim (runtimecomposition, UI-DC-1).
const rustRefusal = "This deployment's agent worker does not support the ado_boards toolkit."

func TestAsSentenceEndsWithExactlyOneFullStop(t *testing.T) {
	cases := map[string]string{
		rustRefusal:                            rustRefusal,
		"The toolkit family is not supported.": "The toolkit family is not supported.",
		"the image does not carry github":      "The image does not carry github.",
		"  trailing dots and space.. ":         "Trailing dots and space.",
		"":                                     "",
		"  . ":                                 "",
	}
	for in, want := range cases {
		if got := AsSentence(in); got != want {
			t.Errorf("AsSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsupportedToolkitTypeErrorKeepsTheOperatorFormAndTheSentence(t *testing.T) {
	err := error(&UnsupportedToolkitTypeError{ToolkitType: "ado_boards", Reason: rustRefusal})
	if !errors.Is(err, ErrUnsupportedToolkitType) {
		t.Fatal("the typed refusal does not match ErrUnsupportedToolkitType")
	}
	if want := "toolkit type cannot be run by this deployment: ado_boards: " + rustRefusal; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if got := UnsupportedToolkitTypeSentence(err); got != rustRefusal {
		t.Fatalf("sentence = %q, want %q", got, rustRefusal)
	}
	// A bare sentinel (toolkit discovery returns one) still reads as a sentence.
	if got := UnsupportedToolkitTypeSentence(ErrUnsupportedToolkitType); got != genericUnsupportedSentence {
		t.Fatalf("bare sentinel sentence = %q", got)
	}
}

// toolVerdict serves the type but not every tool of it.
type toolVerdict struct {
	stubVerdict
	served string
}

func (v toolVerdict) SupportsTool(toolkitType, toolName string) (bool, string) {
	if toolName == v.served {
		return true, ""
	}
	return false, "no " + toolName + " in " + toolkitType
}

// A partial native family (ADR-0027) refuses an unserved TOOL of a type it
// runs, before any write, with the capability's sentence.
func TestRunToolRefusesAToolThePartialFamilyDoesNotServe(t *testing.T) {
	request := validRequest()
	admissions := &stubAdmissions{run: testAdmitted()}
	service := newTestService(t, &stubResolver{inputs: testInputs()},
		toolVerdict{stubVerdict: stubVerdict{supported: true}, served: "something_else"},
		admissions, &stubDispatcher{}, &stubSettlements{}, time.Second)
	_, err := service.RunTool(context.Background(), request)
	var refusal *UnsupportedToolkitTypeError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected *UnsupportedToolkitTypeError, got %T %v", err, err)
	}
	if want := "no " + request.ToolName + " in " + testInputs().ToolkitType; refusal.Reason != want {
		t.Fatalf("reason = %q, want %q", refusal.Reason, want)
	}
	served := newTestService(t, &stubResolver{inputs: testInputs()},
		toolVerdict{stubVerdict: stubVerdict{supported: true}, served: request.ToolName},
		&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, &stubSettlements{}, time.Second)
	if _, err := served.RunTool(context.Background(), request); errors.As(err, &refusal) {
		t.Fatalf("a served tool was refused: %v", err)
	}
}

// The refusal RunTool returns carries the capability reason untouched, so a
// user-facing surface can show it as the sentence it already is.
func TestRunToolRefusalCarriesTheCapabilityReasonAsASentence(t *testing.T) {
	for _, testCase := range []struct {
		name, reason, want string
	}{
		{"a capability reason", rustRefusal, rustRefusal},
		{"no reason given", "", "This deployment cannot run the github toolkit."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := newTestService(t, &stubResolver{inputs: testInputs()},
				stubVerdict{supported: false, reason: testCase.reason},
				&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, &stubSettlements{}, time.Second)
			_, err := service.RunTool(context.Background(), validRequest())
			var refusal *UnsupportedToolkitTypeError
			if !errors.As(err, &refusal) {
				t.Fatalf("expected *UnsupportedToolkitTypeError, got %T %v", err, err)
			}
			if got := UnsupportedToolkitTypeSentence(err); got != testCase.want {
				t.Fatalf("sentence = %q, want %q", got, testCase.want)
			}
		})
	}
}
