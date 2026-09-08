package toolkitnaming_test

// The rule, and the guard that keeps it from being written a sixth time.
//
// The first half pins what the runtime identifier IS — in particular that `_`,
// `.` and `-` are not simply dropped, which is the disagreement that made the
// toolkit details route report a name the runtime never uses.
//
// The second half is a source check, and it is here rather than in a reviewer's
// head because that is exactly how the fifth copy survived: every one of the
// five files passed its own tests, and nothing in the suite compared them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/toolkitnaming"
)

func TestRuntimeNameKeepsTheCharactersTheRuntimeKeeps(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		storedName  string
		toolkitType string
		want        string
	}{
		{
			// The case the details route got wrong. Every one of the three
			// punctuation characters appears, and only the space is dropped.
			name:        "underscore, dot and hyphen all survive; the dot becomes an underscore",
			storedName:  "autotest github-toolkit.v1_beta",
			toolkitType: "github",
			want:        "autotestgithub-toolkit_v1_beta",
		},
		{
			name:        "a plain name is unchanged",
			storedName:  "autotest_github",
			toolkitType: "github",
			want:        "autotest_github",
		},
		{
			name:        "a hyphen is a legal identifier character here",
			storedName:  "my-toolkit",
			toolkitType: "github",
			want:        "my-toolkit",
		},
		{
			name:        "a dot folds into an underscore rather than vanishing",
			storedName:  "wiki.v2",
			toolkitType: "confluence",
			want:        "wiki_v2",
		},
		{
			// Slashes, quotes and non-ASCII letters are not identifier
			// characters for the SDK's tool registry.
			name:        "everything else is dropped",
			storedName:  `a/b c"d\te§f`,
			toolkitType: "custom",
			want:        "abcdtef",
		},
		{
			// The runtime addresses a nameless row by its type. A copy that
			// answered "" here would disagree again, for a different reason.
			name:        "an empty name falls back to the type",
			storedName:  "",
			toolkitType: "artifact",
			want:        "artifact",
		},
		{
			name:        "the type is sanitized too when it is the fallback",
			storedName:  "",
			toolkitType: "open.api",
			want:        "open_api",
		},
		{
			name:        "a name of nothing but punctuation reduces to empty",
			storedName:  "!!!",
			toolkitType: "",
			want:        "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := toolkitnaming.RuntimeName(testCase.storedName, testCase.toolkitType); got != testCase.want {
				t.Errorf("RuntimeName(%q, %q) = %q, want %q",
					testCase.storedName, testCase.toolkitType, got, testCase.want)
			}
		})
	}
}

// TestNoServiceFileHoldsItsOwnCopyOfTheRule walks the service and fails on a
// second definition of the sanitizer.
//
// It looks for the two shapes the five copies actually had: the character class
// itself, and the alphanumeric-only loop the details route used. A new copy
// written a third way would slip past — the check is a ratchet against the
// known regression, not a proof — but every copy that existed is covered, and
// the failure message says where the one rule lives.
func TestNoServiceFileHoldsItsOwnCopyOfTheRule(t *testing.T) {
	serviceRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the service root: %v", err)
	}
	packageDir := filepath.Join(serviceRoot, "internal", "toolkitnaming")

	var offenders []string
	err = filepath.Walk(serviceRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// The naming package itself owns the rule, and the generated API
			// surface is not hand-written code anyone could copy it into.
			if path == packageDir || info.Name() == "generated" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path) // #nosec G304 -- a path this walk produced
		if readErr != nil {
			return readErr
		}
		text := string(source)
		if strings.Contains(text, "[^a-zA-Z0-9_.-]") ||
			strings.Contains(text, `(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')`) {
			relative, _ := filepath.Rel(serviceRoot, path)
			offenders = append(offenders, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the service: %v", err)
	}
	if len(offenders) != 0 {
		t.Errorf("these files hold their own copy of the toolkit-name rule: %v\n"+
			"call internal/toolkitnaming.RuntimeName instead — five copies is how the "+
			"details route came to report a name the runtime does not use", offenders)
	}
}
