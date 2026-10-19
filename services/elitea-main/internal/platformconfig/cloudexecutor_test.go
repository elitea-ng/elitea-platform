package platformconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCloudExecutorDefaultsAreOffWithTheADRValues(t *testing.T) {
	got := CloudExecutorSettingsFromValues(Values{})
	want := CloudExecutorSettings{
		Enabled: false, Shell: true, Network: true, NetworkAllowlist: []string{}, HistoryFullAllowed: false,
		MaxSegmentMB: 200, MaxFileMB: 25, MaxFiles: 20000, MaxUnpackedMB: 2048, MaxProjectWorkspaceGB: 50,
		IdleRetentionDays: 30, ReturnGraceDays: 7,
		Exclusions: []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", "id_ed25519*", ".npmrc", ".pypirc",
			".netrc", ".aws/**", ".ssh/**", "*.p12", "*.pfx", "*.kdbx", "*.jks", "*.keystore",
			"credentials*.json"},
		AllowUserInclude: true, EnvHarvest: false,
		EnvDeny: []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TERM*", "LANG",
			"LC_*", "SSH_*", "GPG_*", "XPC_*", "__CF*", "DISPLAY", "ELITEA_*"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestCloudExecutorOverlayKeepsBounds(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"enabled": true, "project_default_enabled": true, "shell": false, "network": false, "history_full_allowed": true,
		"network_allowlist": ["GitHub.com", "*.npmjs.org", "not a host", "localhost", "github.com"],
		"max_segment_mb": 100, "max_file_mb": 999999, "max_files": "many", "max_unpacked_mb": 0,
		"max_project_workspace_gb": 5, "idle_retention_days": 3, "return_grace_days": 0}`), &values); err != nil {
		t.Fatal(err)
	}
	got := CloudExecutorSettingsFromValues(values)
	if !got.Enabled || !got.ProjectDefaultEnabled || got.Shell || got.Network || !got.HistoryFullAllowed ||
		!reflect.DeepEqual(got.NetworkAllowlist, []string{"github.com", "*.npmjs.org"}) ||
		got.MaxSegmentMB != 100 || got.MaxFileMB != 25 || got.MaxFiles != 20000 || got.MaxUnpackedMB != 2048 ||
		got.MaxProjectWorkspaceGB != 5 || got.IdleRetentionDays != 3 || got.ReturnGraceDays != 0 {
		t.Fatalf("overlay = %+v", got)
	}
}

func TestCloudExecutorAmendmentKeys(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{"exclusions": [".env", "secrets/**", ""], "allow_user_include": false,
		"env_harvest": true, "env_deny": ["SSH_*"], "env_allow": ["DATABASE_URL"]}`), &values); err != nil {
		t.Fatal(err)
	}
	got := CloudExecutorSettingsFromValues(values)
	if !reflect.DeepEqual(got.Exclusions, []string{".env", "secrets/**"}) || got.AllowUserInclude || !got.EnvHarvest ||
		!reflect.DeepEqual(got.EnvDeny, []string{"SSH_*"}) || got.EnvAllow == nil || (*got.EnvAllow)[0] != "DATABASE_URL" {
		t.Fatalf("overlay = %+v", got)
	}
	// An unset env_allow stays unset (every name allowed but the denied ones).
	if CloudExecutorSettingsFromValues(Values{}).EnvAllow != nil {
		t.Fatalf("env_allow is set by default")
	}
}
