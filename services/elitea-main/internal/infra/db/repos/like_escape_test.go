package repos

import "testing"

func TestEscapeLikeEscapesEveryMetacharacterOnce(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"plain":     "plain",
		"50%":       `50\%`,
		"a_b":       `a\_b`,
		`C:\path`:   `C:\\path`,
		`\%_`:       `\\\%\_`,
		`100\%_off`: `100\\\%\_off`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
	if got := likePrefix("run_1"); got != `run\_1%` {
		t.Errorf("likePrefix kept its own escaping: %q", got)
	}
}
