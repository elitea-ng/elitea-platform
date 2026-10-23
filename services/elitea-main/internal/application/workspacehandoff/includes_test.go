package workspacehandoff

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func amendedCeiling() platformconfig.CloudExecutorSettings {
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	ceiling.Enabled, ceiling.ProjectDefaultEnabled = true, true
	return ceiling
}

func TestNarrowMergesExclusionsAndNeverDropsAPlatformPattern(t *testing.T) {
	got := Narrow(amendedCeiling(), ProjectSetting{ExclusionsAdd: &[]string{"secrets/**", ".env"}}, true, true)
	if got.Exclusions[0] != ".env" || got.Exclusions[len(got.Exclusions)-1] != "secrets/**" ||
		len(got.Exclusions) != len(platformconfig.DefaultCloudExecutorExclusions())+1 {
		t.Fatalf("exclusions = %v, want the platform list then the project addition", got.Exclusions)
	}
	// A project list that leaves platform patterns out removes nothing.
	got = Narrow(amendedCeiling(), ProjectSetting{ExclusionsAdd: &[]string{}}, true, true)
	if !reflect.DeepEqual(got.Exclusions, platformconfig.DefaultCloudExecutorExclusions()) {
		t.Fatalf("exclusions = %v", got.Exclusions)
	}
	// The split: a project addition shows up only in ExclusionsProject, and
	// one the platform already lists shows up in neither twice.
	got = Narrow(amendedCeiling(), ProjectSetting{ExclusionsAdd: &[]string{"secrets/**", ".env", "secrets/**"}}, true, true)
	if !reflect.DeepEqual(got.ExclusionsProject, []string{"secrets/**"}) ||
		!reflect.DeepEqual(got.ExclusionsPlatform, platformconfig.DefaultCloudExecutorExclusions()) {
		t.Fatalf("split = platform %v, project %v", got.ExclusionsPlatform, got.ExclusionsProject)
	}
	if got = Narrow(amendedCeiling(), ProjectSetting{}, true, true); got.ExclusionsProject == nil || len(got.ExclusionsProject) != 0 {
		t.Fatalf("no project additions = %#v, want an empty list", got.ExclusionsProject)
	}
	denied := Narrow(amendedCeiling(), ProjectSetting{EnvDenyAdd: &[]string{"AWS_*"}}, true, true).EnvDeny
	if denied[0] != "PATH" || denied[len(denied)-1] != "AWS_*" {
		t.Fatalf("env deny = %v", denied)
	}
}

func TestNarrowIncludeAndHarvestSwitches(t *testing.T) {
	ceiling := amendedCeiling()
	if got := Narrow(ceiling, ProjectSetting{}, true, true); !got.AllowUserInclude || got.EnvHarvest {
		t.Fatalf("defaults: include %t harvest %t, want true/false", got.AllowUserInclude, got.EnvHarvest)
	}
	if got := Narrow(ceiling, ProjectSetting{AllowUserInclude: boolPtr(false)}, true, true); got.AllowUserInclude {
		t.Fatalf("a project could not turn include off")
	}
	if got := Narrow(ceiling, ProjectSetting{EnvHarvest: boolPtr(true)}, true, true); got.EnvHarvest {
		t.Fatalf("a project turned the harvest on under a ceiling that has it off")
	}
	ceiling.EnvHarvest = true
	if got := Narrow(ceiling, ProjectSetting{}, true, true); !got.EnvHarvest {
		t.Fatalf("the platform harvest did not apply")
	}
	if got := Narrow(ceiling, ProjectSetting{EnvHarvest: boolPtr(false)}, true, true); got.EnvHarvest {
		t.Fatalf("a project could not turn the harvest off")
	}
	// env_allow: unset allows all; a project narrows a set platform list.
	if got := Narrow(ceiling, ProjectSetting{}, true, true); got.EnvAllow != nil {
		t.Fatalf("env_allow = %v, want unset", *got.EnvAllow)
	}
	ceiling.EnvAllow = &[]string{"DATABASE_URL", "API_TOKEN"}
	got := Narrow(ceiling, ProjectSetting{EnvAllow: &[]string{"API_TOKEN", "OTHER"}}, true, true)
	if got.EnvAllow == nil || !reflect.DeepEqual(*got.EnvAllow, []string{"API_TOKEN"}) {
		t.Fatalf("env_allow = %v, want the intersection", got.EnvAllow)
	}
}

func TestMatchPathPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"**/.env", "./.env", true},
		{"**/.env", "./api/.env", true},
		{".env", "./deep/dir/.env", true},
		{"*.pem", "./certs/server.pem", true},
		{".aws/**", "./.aws/credentials", true},
		{".aws/**", "./src/.aws/credentials", false},
		{"secrets/*.json", "./secrets/a.json", true},
		{"secrets/*.json", "./secrets/x/a.json", false},
		{"~/.ssh/**", "./.ssh/id_rsa", false},
		{"/etc/**", "./etc/passwd", false},
		{"**/.env", "./.env.local", false},
		// A directory pattern covers everything under it (gitignore).
		{"secrets/", "./secrets/a.json", true},
		{"secrets/", "./secrets/deep/x/key.pem", true},
		{"secrets/", "./src/secrets/key.pem", true},
		{"secrets/", "./secrets", false}, // a FILE named secrets
		{"secrets/", "./secretsfile", false},
		{".ssh", "./.ssh/id_rsa", true},
		{"config/secrets/", "./config/secrets/a", true},
		{"config/secrets/", "./other/config/secrets/a", false},
		// Case folds on both sides (case-insensitive desktop file systems).
		{".ssh/**", "./.SSH/id_rsa", true},
		{"**/.ENV", "./api/.env", true},
		{"*.PEM", "./certs/Server.Pem", true},
		{"Secrets/", "./SECRETS/a.json", true},
	} {
		if got := MatchPathPattern(tc.pattern, tc.path); got != tc.want {
			t.Fatalf("MatchPathPattern(%q, %q) = %t, want %t", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestIncludesAreRefusedOffPolicyAndPathDenyIsNeverIncludable(t *testing.T) {
	policy := Policy{AllowUserInclude: true, PathDeny: []string{"**/.env.production", "~/.ssh/**"}}
	if err := checkIncludes([]string{"./.env"}, policy); err != nil {
		t.Fatalf("an allowed include = %v", err)
	}
	var denied *PathDeniedError
	if err := checkIncludes([]string{"./.env", "./app/.env.production"}, policy); !errors.As(err, &denied) ||
		!reflect.DeepEqual(denied.Paths, []string{"./app/.env.production"}) {
		t.Fatalf("a path_deny include = %v", err)
	}
	policy.AllowUserInclude = false
	if err := checkIncludes([]string{"./.env"}, policy); !errors.Is(err, ErrIncludeNotAllowed) {
		t.Fatalf("an include with the policy off = %v", err)
	}
	if err := checkIncludes(nil, policy); err != nil {
		t.Fatalf("no include with the policy off = %v", err)
	}
}

func TestEnvHarvestRules(t *testing.T) {
	policy := Narrow(amendedCeiling(), ProjectSetting{}, true, true)
	if _, err := checkEnv(map[string]string{"DATABASE_URL": "postgres://x"}, policy); !errors.Is(err, ErrEnvHarvestDisabled) {
		t.Fatalf("env with the harvest off = %v", err)
	}
	if names, err := checkEnv(map[string]string{}, policy); err != nil || len(names) != 0 {
		t.Fatalf("an empty env (clear) with the harvest off = %v, %v", names, err)
	}
	policy.EnvHarvest = true
	names, err := checkEnv(map[string]string{"DATABASE_URL": "postgres://x", "API_TOKEN": "t"}, policy)
	if err != nil || !reflect.DeepEqual(names, []string{"API_TOKEN", "DATABASE_URL"}) {
		t.Fatalf("an allowed env = %v, %v", names, err)
	}
	var denied *EnvNameDeniedError
	if _, err := checkEnv(map[string]string{"SSH_AUTH_SOCK": "/tmp/x", "PATH": "/bin", "OK": "1"}, policy); !errors.As(err, &denied) ||
		!reflect.DeepEqual(denied.Names, []string{"PATH", "SSH_AUTH_SOCK"}) {
		t.Fatalf("denied names = %v", err)
	}
	allow := []string{"DATABASE_URL"}
	policy.EnvAllow = &allow
	if _, err := checkEnv(map[string]string{"API_TOKEN": "t"}, policy); !errors.As(err, &denied) || denied.Names[0] != "API_TOKEN" {
		t.Fatalf("a name outside env_allow = %v", err)
	}
	policy.EnvAllow = nil
	many := map[string]string{}
	for i := 0; i < 201; i++ {
		many["V"+strings.Repeat("A", i%5)+string(rune('A'+i%26))+strings.Repeat("B", i/26)] = "x"
	}
	if len(many) <= 200 {
		t.Fatalf("fixture has %d names", len(many))
	}
	if _, err := checkEnv(many, policy); !errors.Is(err, ErrEnvTooLarge) {
		t.Fatalf("201 variables = %v", err)
	}
	if _, err := checkEnv(map[string]string{"BIG": strings.Repeat("x", 64*1024)}, policy); !errors.Is(err, ErrEnvTooLarge) {
		t.Fatalf("a 64 KiB value = %v", err)
	}
	if _, err := checkEnv(map[string]string{"bad-name": "x"}, policy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid name = %v", err)
	}
}

// An include is checked against path_deny case-insensitively and by
// directory: `./.SSH/id_rsa` is denied by `.ssh/**`, and anything under a
// `secrets/` deny entry is denied.
func TestIncludesDeniedByCaseAndDirectory(t *testing.T) {
	policy := Policy{AllowUserInclude: true, PathDeny: []string{".ssh/**", "secrets/"}}
	err := checkIncludes([]string{"./.SSH/id_rsa", "./secrets/prod/key.pem", "./src/main.go"}, policy)
	var denied *PathDeniedError
	if !errors.As(err, &denied) || !reflect.DeepEqual(denied.Paths, []string{"./.SSH/id_rsa", "./secrets/prod/key.pem"}) {
		t.Fatalf("err = %v, want the two paths denied", err)
	}
}
