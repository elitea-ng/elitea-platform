package discovery

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The key set and order are a contract (ADR-0025 decision 1/6): a client
// reads these names. Adding a key is additive; renaming or removing one is a
// client_contract major bump, and this test is where that shows.
func TestDocumentKeySetIsPinned(t *testing.T) {
	body, err := json.Marshal(Document{MinClientVersion: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"server_version", "client_contract", "deployment_kind", "display_name",
		"brand_pack_url", "native_auth", "client_policy", "min_client_version",
		"attachments",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("discovery keys = %v, want %v", keys, want)
	}

	policy, _ := json.Marshal(PublicPolicy{})
	var pm map[string]any
	_ = json.Unmarshal(policy, &pm)
	publicKeys := []string{"require_device_lock", "offline_enabled", "min_client_version",
		"allow_share_out", "allow_share_in", "allow_cloud_stt", "notification_preview",
		"allow_notification_actions", "allow_system_surfaces", "local_work_allowed"}
	for _, k := range publicKeys {
		if _, ok := pm[k]; !ok || len(pm) != len(publicKeys) {
			t.Fatalf("client_policy keys = %v", pm)
		}
	}

	attachments, _ := json.Marshal(AttachmentPolicy{})
	var am map[string]any
	_ = json.Unmarshal(attachments, &am)
	attachmentKeys := []string{
		"max_files", "max_total_bytes", "max_file_bytes", "max_image_bytes", "chunk_bytes",
		"accepted_extensions", "max_extract_bytes", "inline_image_max_bytes", "inline_image_formats",
		"inline_image_downscale",
	}
	for _, k := range attachmentKeys {
		if _, ok := am[k]; !ok || len(am) != len(attachmentKeys) {
			t.Fatalf("attachments keys = %v", am)
		}
	}

	na, _ := json.Marshal(NativeAuth{})
	var nm map[string]any
	_ = json.Unmarshal(na, &nm)
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "revocation_endpoint", "code_challenge_methods_supported"} {
		if _, ok := nm[k]; !ok || len(nm) != 5 {
			t.Fatalf("native_auth keys = %v", nm)
		}
	}
}

func TestMajorMinor(t *testing.T) {
	for in, want := range map[string]string{
		"v1.62.3":      "1.62",
		"1.62.0-rc.1":  "1.62",
		"2.0":          "2.0",
		"v3.1-beta":    "3.1",
		" 1.2.3 ":      "1.2",
		"dev":          "dev",
		"":             "dev",
		"1":            "dev",
		"x.y.z":        "dev",
		"1.+2.0":       "dev",
		"1.-2.0":       "dev",
		"(devel)":      "dev",
		"1.2.3+abc123": "1.2",
	} {
		if got := MajorMinor(in); got != want {
			t.Errorf("MajorMinor(%q) = %q, want %q", in, got, want)
		}
	}
}
