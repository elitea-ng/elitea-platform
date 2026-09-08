package publicproject

import (
	"strings"
	"testing"
)

func lookupFrom(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := pairs[name]
		return value, ok
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		env        map[string]string
		wantID     int
		wantSource string
		wantDepr   []string
	}{
		{
			name:   "nothing set gives the default",
			env:    map[string]string{},
			wantID: Default,
		},
		{
			name:       "the canonical name wins",
			env:        map[string]string{Canonical: "7"},
			wantID:     7,
			wantSource: Canonical,
		},
		{
			name:       "a deprecated name still resolves and is reported",
			env:        map[string]string{"PUBLIC_PROJECT_ID": "9"},
			wantID:     9,
			wantSource: "PUBLIC_PROJECT_ID",
			wantDepr:   []string{"PUBLIC_PROJECT_ID"},
		},
		{
			name:       "SHARED_PROJECT_ID is an alias, not a second project",
			env:        map[string]string{"SHARED_PROJECT_ID": "4"},
			wantID:     4,
			wantSource: "SHARED_PROJECT_ID",
			wantDepr:   []string{"SHARED_PROJECT_ID"},
		},
		{
			name: "agreeing aliases are accepted and all reported",
			env: map[string]string{
				Canonical:           "3",
				"AI_PROJECT_ID":     "3",
				"PUBLIC_PROJECT_ID": "3",
				"SHARED_PROJECT_ID": "3",
			},
			wantID:     3,
			wantSource: Canonical,
			wantDepr:   []string{"AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"},
		},
		{
			name:       "an empty deprecated name is not a value",
			env:        map[string]string{Canonical: "5", "PUBLIC_PROJECT_ID": ""},
			wantID:     5,
			wantSource: Canonical,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Resolve(lookupFrom(tc.env))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("ID = %d, want %d", got.ID, tc.wantID)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
			if strings.Join(got.DeprecatedNames, ",") != strings.Join(tc.wantDepr, ",") {
				t.Errorf("DeprecatedNames = %v, want %v", got.DeprecatedNames, tc.wantDepr)
			}
		})
	}
}

func TestResolveRefusesDisagreement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "canonical against a deprecated alias",
			env:  map[string]string{Canonical: "1", "PUBLIC_PROJECT_ID": "2"},
			want: "name different public projects",
		},
		{
			name: "two deprecated aliases",
			env:  map[string]string{"PUBLIC_PROJECT_ID": "1", "SHARED_PROJECT_ID": "4"},
			want: "name different public projects",
		},
		{
			name: "a value that is not a project id",
			env:  map[string]string{Canonical: "not-a-number"},
			want: "is invalid",
		},
		{
			name: "zero is not a project id",
			env:  map[string]string{"SHARED_PROJECT_ID": "0"},
			want: "is invalid",
		},
		{
			name: "a negative id",
			env:  map[string]string{"AI_PROJECT_ID": "-3"},
			want: "is invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(lookupFrom(tc.env))
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestResolveRequiresLookup(t *testing.T) {
	t.Parallel()
	if _, err := Resolve(nil); err == nil {
		t.Fatal("expected a refusal for a nil lookup")
	}
}

func TestID(t *testing.T) {
	t.Run("unset gives the default", func(t *testing.T) {
		for _, name := range names() {
			t.Setenv(name, "")
		}
		if got := ID(); got != Default {
			t.Errorf("ID() = %d, want %d", got, Default)
		}
	})

	t.Run("the canonical name wins over an alias", func(t *testing.T) {
		for _, name := range names() {
			t.Setenv(name, "")
		}
		t.Setenv(Canonical, "11")
		t.Setenv("PUBLIC_PROJECT_ID", "22")
		if got := ID(); got != 11 {
			t.Errorf("ID() = %d, want 11", got)
		}
		if got := IDString(); got != "11" {
			t.Errorf("IDString() = %q, want %q", got, "11")
		}
	})

	t.Run("an unusable value falls through rather than failing a request", func(t *testing.T) {
		for _, name := range names() {
			t.Setenv(name, "")
		}
		t.Setenv(Canonical, "not-a-number")
		t.Setenv("SHARED_PROJECT_ID", "6")
		if got := ID(); got != 6 {
			t.Errorf("ID() = %d, want 6", got)
		}
	})
}
