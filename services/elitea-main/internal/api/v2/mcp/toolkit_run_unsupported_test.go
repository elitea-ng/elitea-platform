package mcp

import (
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// UI-DC-1: both unsupported-toolkit paths read as whole sentences. The
// capability reason already ends in a full stop; the old wording put the
// operator prefix in front and ". Nothing was executed" after it, which gave
// "…ado_boards toolkit.. Nothing was executed".
func TestToolkitCallWordsAnUnsupportedToolkitAsSentences(t *testing.T) {
	const reason = "This deployment's agent worker does not support the ado_boards toolkit."
	const tail = " Nothing was executed and nothing was changed."
	cases := []struct {
		name    string
		outcome toolkitcalltoolapp.RunOutcome
		err     error
		want    string
	}{
		{
			name: "refused before admission",
			err:  &toolkitcalltoolapp.UnsupportedToolkitTypeError{ToolkitType: "ado_boards", Reason: reason},
			want: "'github_get_issue' cannot run on this deployment. " + reason + tail,
		},
		{
			name: "refused by the worker, with its own full stop",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e7", Status: toolkitcalltoolapp.RunStatusUnsupportedToolkit,
				ErrorMessage: "The toolkit family is not supported.",
			},
			want: "'github_get_issue' cannot run on this deployment. The toolkit family is not supported." + tail,
		},
		{
			name: "refused by the worker with no message",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e8", Status: toolkitcalltoolapp.RunStatusUnsupportedToolkit,
			},
			want: "'github_get_issue' cannot run on this deployment. The agent worker refused this toolkit." + tail,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runs := &recordingToolkitRun{outcome: testCase.outcome, err: testCase.err}
			result := callGetIssue(t, newToolkitRunRouter(t, &recordingStart{}, runs), `{}`)
			if result["isError"] != true {
				t.Fatalf("isError = %v, want true", result["isError"])
			}
			if got := textOf(t, result); got != testCase.want {
				t.Fatalf("text = %q\nwant   %q", got, testCase.want)
			}
		})
	}
}
