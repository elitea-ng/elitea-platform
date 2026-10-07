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
	want := DefaultNativeClientPolicy()
	want.RequireDeviceLock, want.IdleLockSeconds, want.AllowScreenshots = true, 300, false
	want.OfflineRetentionDays, want.OfflineMaxMB, want.OfflineAttachments, want.MinClientVersion = 0, 64, false, "1.4.0"
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
		`"offline_retention_days":30,"offline_max_mb":512,"offline_attachments":true,"min_client_version":"",` +
		`"allow_share_out":true,"allow_share_in":true,"allow_cloud_stt":false,"notification_preview":"none",` +
		`"allow_notification_actions":true,"allow_system_surfaces":false}`
	if string(body) != want {
		t.Fatalf("client_policy JSON = %s\nwant %s", body, want)
	}
}

// TestNativeClientPolicyDataControlDefaults pins the client contract 1.3
// defaults: what a deployment that never saved the section allows. Sharing and
// notification actions are on (SaaS behaviour); cloud speech-to-text and
// titles on system surfaces (widgets, quick actions) are off, and a
// notification shows no preview.
func TestNativeClientPolicyDataControlDefaults(t *testing.T) {
	got := DefaultNativeClientPolicy()
	if !got.AllowShareOut || !got.AllowShareIn || !got.AllowNotificationActions {
		t.Errorf("share out/in and notification actions default on, got %+v", got)
	}
	if got.AllowCloudSTT || got.AllowSystemSurfaces {
		t.Errorf("cloud STT and system-surface titles default off, got %+v", got)
	}
	if got.NotificationPreview != NotificationPreviewNone {
		t.Errorf("notification_preview default = %q, want none", got.NotificationPreview)
	}
}

func TestNativeClientPolicyDataControlOverlay(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"allow_share_out": false, "allow_share_in": false, "allow_cloud_stt": true,
		"notification_preview": "title", "allow_notification_actions": false,
		"allow_system_surfaces": true}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values)
	if got.AllowShareOut || got.AllowShareIn || !got.AllowCloudSTT || got.NotificationPreview != NotificationPreviewTitle ||
		got.AllowNotificationActions || !got.AllowSystemSurfaces {
		t.Fatalf("overlay = %+v", got)
	}
	for _, junk := range []string{`{"notification_preview": "body"}`, `{"notification_preview": 1}`, `{"notification_preview": " Title "}`} {
		values = nil
		if err := json.Unmarshal([]byte(junk), &values); err != nil {
			t.Fatal(err)
		}
		if got := NativeClientPolicyFromValues(values); got.NotificationPreview != NotificationPreviewNone {
			t.Errorf("%s: notification_preview = %q, want the default none", junk, got.NotificationPreview)
		}
	}
}
