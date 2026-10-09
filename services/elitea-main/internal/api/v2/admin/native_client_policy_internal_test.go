package admin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateNativeClientPolicyValues(t *testing.T) {
	cases := []struct {
		name   string
		values string
		reason string // substring; "" means accepted
	}{
		{"empty", `{}`, ""},
		{"all valid", `{"require_device_lock":true,"idle_lock_seconds":300,"allow_screenshots":false,
			"offline_retention_days":90,"offline_max_mb":0,"offline_attachments":true,"min_client_version":"1.4.0-rc.1"}`, ""},
		{"empty version clears the minimum", `{"min_client_version":""}`, ""},
		{"idle lock above the cap", `{"idle_lock_seconds":86401}`, "idle_lock_seconds"},
		{"negative idle lock", `{"idle_lock_seconds":-1}`, "idle_lock_seconds"},
		{"fractional retention", `{"offline_retention_days":1.5}`, "offline_retention_days"},
		{"retention above the cap", `{"offline_retention_days":91}`, "offline_retention_days"},
		{"storage above the cap", `{"offline_max_mb":102401}`, "offline_max_mb"},
		{"version not semver", `{"min_client_version":"1.4"}`, "min_client_version"},
		{"version with junk", `{"min_client_version":"latest"}`, "min_client_version"},
		{"data controls valid", `{"allow_share_out":false,"allow_share_in":false,"allow_cloud_stt":true,
			"notification_preview":"title","allow_notification_actions":false,"allow_system_surfaces":true}`, ""},
		{"preview none", `{"notification_preview":"none"}`, ""},
		{"preview body is not offered", `{"notification_preview":"body"}`, "notification_preview"},
		{"preview not a string", `{"notification_preview":true}`, "notification_preview"},
		{"local work valid", `{"local_work_allowed":true,"local_work_shell":false,
			"local_work_max_sandbox_mode":"full-access","local_work_network":true,
			"local_work_command_allow":["cargo test"],"local_work_command_deny":["rm -rf *"],
			"local_work_path_deny":["**/.env"],"local_work_local_mcp":true,"local_work_local_index":false,
			"local_work_cloud_sync":true,"local_work_memory_write":false}`, ""},
		{"sandbox mode unknown", `{"local_work_max_sandbox_mode":"root"}`, "local_work_max_sandbox_mode"},
		{"sandbox mode not exact", `{"local_work_max_sandbox_mode":"Read-Only"}`, "local_work_max_sandbox_mode"},
		{"blank pattern", `{"local_work_command_deny":["ok","  "]}`, "local_work_command_deny"},
		{"control character in pattern", `{"local_work_path_deny":["a\nb"]}`, "local_work_path_deny"},
		{"pattern not a string", `{"local_work_command_allow":[1]}`, "local_work_command_allow"},
		{"pattern too long", `{"local_work_command_allow":["` + strings.Repeat("x", 513) + `"]}`, "local_work_command_allow"},
		{"too many patterns", `{"local_work_path_deny":[` + strings.TrimSuffix(strings.Repeat(`"p",`, 201), ",") + `]}`, "local_work_path_deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var values map[string]any
			if err := json.Unmarshal([]byte(tc.values), &values); err != nil {
				t.Fatal(err)
			}
			got := validateNativeClientPolicyValues(values)
			if tc.reason == "" && got != "" {
				t.Fatalf("refused: %s", got)
			}
			if tc.reason != "" && !strings.Contains(got, tc.reason) {
				t.Fatalf("reason = %q, want one naming %q", got, tc.reason)
			}
		})
	}
}

// TestNativeClientPolicySectionIsLiveAndShared — no unavailable_reason (three
// consumers read it), the permission both native surfaces share (decision 6),
// and one schema field per policy key.
func TestNativeClientPolicySectionIsLiveAndShared(t *testing.T) {
	section, ok := findConfigSection("native_client_policy")
	if !ok {
		t.Fatal("native_client_policy is not declared in configSections()")
	}
	if _, withheld := section.raw["unavailable_reason"]; withheld {
		t.Fatal("native_client_policy must be live")
	}
	if got := section.raw["required_permission"]; got != "configuration.native_clients" {
		t.Fatalf("required_permission = %v, want configuration.native_clients", got)
	}
	if _, onFeatures := section.raw["page"]; onFeatures {
		t.Fatal("native_client_policy belongs on Configuration, not Features")
	}
	keys := map[string]bool{}
	for _, field := range section.fields {
		keys[field["key"].(string)] = true
	}
	for _, key := range []string{"require_device_lock", "idle_lock_seconds", "allow_screenshots",
		"offline_retention_days", "offline_max_mb", "offline_attachments", "min_client_version",
		"allow_share_out", "allow_share_in", "allow_cloud_stt", "notification_preview",
		"allow_notification_actions", "allow_system_surfaces",
		"local_work_allowed", "local_work_shell", "local_work_max_sandbox_mode", "local_work_network",
		"local_work_command_allow", "local_work_command_deny", "local_work_path_deny",
		"local_work_local_mcp", "local_work_local_index", "local_work_cloud_sync", "local_work_memory_write"} {
		if !keys[key] {
			t.Errorf("field %q missing", key)
		}
	}
	if len(keys) != 24 {
		t.Errorf("section declares %d fields, want the ADR's 7, contract 1.3's 6 and contract 1.5's 11", len(keys))
	}
	for _, field := range section.fields {
		if field["key"] == "notification_preview" {
			if enum, _ := field["enum"].([]string); len(enum) != 2 || enum[0] != "none" || enum[1] != "title" {
				t.Errorf("notification_preview enum = %v, want [none title]", field["enum"])
			}
		}
		if field["key"] == "local_work_max_sandbox_mode" {
			enum, _ := field["enum"].([]string)
			if strings.Join(enum, ",") != "read-only,workspace-write,full-access" {
				t.Errorf("local_work_max_sandbox_mode enum = %v", field["enum"])
			}
		}
		// Every local_work field but the switch itself shows only when local
		// work is allowed.
		if key, _ := field["key"].(string); strings.HasPrefix(key, "local_work_") && key != "local_work_allowed" {
			visible, _ := field["visible_when"].(map[string]any)
			if visible["field"] != "local_work_allowed" || visible["value"] != true {
				t.Errorf("%s visible_when = %v, want local_work_allowed=true", key, field["visible_when"])
			}
		}
	}
}
