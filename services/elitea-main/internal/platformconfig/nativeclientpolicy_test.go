package platformconfig

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNativeClientPolicyDefaultsOnEmptySection(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{})
	if got != DefaultNativeClientPolicy() {
		t.Fatalf("empty section = %+v, want defaults", got)
	}
	if !got.OfflineEnabled() {
		t.Fatal("offline must be enabled by default (retention 30)")
	}
	policy, err := LoadNativeClientPolicy(context.Background(), nil)
	if err != nil || policy != DefaultNativeClientPolicy() {
		t.Fatalf("nil pool = %+v, %v; want defaults, nil", policy, err)
	}
}

func TestNativeClientPolicyOverlay(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"require_device_lock": true, "idle_lock_seconds": 300, "allow_screenshots": false,
		"offline_retention_days": 0, "offline_max_mb": 64, "offline_attachments": false,
		"min_client_version": " 1.4.0 "}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values)
	want := NativeClientPolicy{
		RequireDeviceLock: true, IdleLockSeconds: 300, AllowScreenshots: false,
		OfflineRetentionDays: 0, OfflineMaxMB: 64, OfflineAttachments: false, MinClientVersion: "1.4.0",
	}
	if got != want {
		t.Fatalf("overlay = %+v, want %+v", got, want)
	}
	if got.OfflineEnabled() {
		t.Fatal("retention 0 disables offline storage")
	}
}

func TestNativeClientPolicyIgnoresOutOfRangeAndWrongType(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"require_device_lock": "yes", "idle_lock_seconds": 86401, "offline_retention_days": 91,
		"offline_max_mb": -1, "min_client_version": "one", "allow_screenshots": 0,
		"offline_attachments": null}`), &values); err != nil {
		t.Fatal(err)
	}
	if got := NativeClientPolicyFromValues(values); got != DefaultNativeClientPolicy() {
		t.Fatalf("hand-written junk must keep defaults, got %+v", got)
	}
	values = Values{KeyNativeIdleLockSeconds: 1.5}
	if got := NativeClientPolicyFromValues(values); got.IdleLockSeconds != 0 {
		t.Fatalf("non-integral seconds must be absent, got %d", got.IdleLockSeconds)
	}
}

func TestNativeClientPolicyJSONKeyOrder(t *testing.T) {
	body, err := json.Marshal(DefaultNativeClientPolicy())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"require_device_lock":false,"idle_lock_seconds":0,"allow_screenshots":true,` +
		`"offline_retention_days":30,"offline_max_mb":512,"offline_attachments":true,"min_client_version":""}`
	if string(body) != want {
		t.Fatalf("client_policy JSON = %s\nwant %s", body, want)
	}
}
