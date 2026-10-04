package nativeauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

type deviceList struct {
	Devices []nativeapi.UserDevice `json:"devices"`
}

type adminDeviceList struct {
	Rows  []nativeapi.AdminDevice `json:"rows"`
	Total int64                   `json:"total"`
}

func (s *stack) bearer(method, target, accessToken string) (int, []byte) {
	recorder := s.do(method, target, nil, withHeader("Authorization", "Bearer "+accessToken))
	return recorder.Code, recorder.Body.Bytes()
}

// ADR-0025 decision 4 (WP3): a user lists and revokes their OWN devices; the
// device in use is marked current; another user's device is a 404; a revoked
// device's next call is device_revoked.
func TestNativeDeviceRegistryForUsersAndAdministrators(t *testing.T) {
	s := newStack(t)
	alice := s.seedUser("alice.devices@example.test")
	bob := s.seedUser("bob.devices@example.test")
	phone := s.exchange(s.signIn(alice, "alice.devices@example.test"))
	laptop := s.exchange(s.signIn(alice, "alice.devices@example.test"))
	bobs := s.exchange(s.signIn(bob, "bob.devices@example.test"))

	status, body := s.bearer(http.MethodGet, nativeapi.DevicesPath, phone.AccessToken)
	var mine deviceList
	if status != http.StatusOK || json.Unmarshal(body, &mine) != nil || len(mine.Devices) != 2 {
		t.Fatalf("alice's devices = %d %s", status, body)
	}
	currents := 0
	for _, device := range mine.Devices {
		if device.ID == bobs.DeviceID {
			t.Fatal("a user saw another user's device")
		}
		if device.Current {
			currents++
			if device.ID != phone.DeviceID {
				t.Fatalf("current = %s, want the phone %s", device.ID, phone.DeviceID)
			}
		}
		if device.ClientName != "Conformance" || device.Platform != "ios" || device.DeviceName != "Alex's Phone" {
			t.Fatalf("device = %+v", device)
		}
	}
	if currents != 1 {
		t.Fatalf("%d devices marked current, want 1", currents)
	}

	// Another user's device: 404, nothing revoked.
	if status, _ := s.bearer(http.MethodDelete, nativeapi.DevicesPath+"/"+bobs.DeviceID, phone.AccessToken); status != http.StatusNotFound {
		t.Fatalf("revoking bob's device as alice = %d, want 404", status)
	}
	if s.family(bobs.DeviceID).revokedAt != nil {
		t.Fatal("bob's device was revoked by alice")
	}
	// Her own laptop: 204, and the laptop is cut off with device_revoked.
	if status, _ := s.bearer(http.MethodDelete, nativeapi.DevicesPath+"/"+laptop.DeviceID, phone.AccessToken); status != http.StatusNoContent {
		t.Fatalf("revoking her own laptop = %d", status)
	}
	if reason(s.family(laptop.DeviceID)) != domain.ReasonUser {
		t.Fatalf("laptop = %+v", s.family(laptop.DeviceID))
	}
	cut := s.whoami(laptop.AccessToken)
	if cut.Code != http.StatusUnauthorized || !strings.Contains(cut.Body.String(), `"error":"device_revoked"`) {
		t.Fatalf("laptop after revoke = %d %s", cut.Code, cut.Body.String())
	}
	// include_revoked shows it; the default list does not.
	_, body = s.bearer(http.MethodGet, nativeapi.DevicesPath, phone.AccessToken)
	_ = json.Unmarshal(body, &mine)
	if len(mine.Devices) != 1 {
		t.Fatalf("default list = %d devices, want 1", len(mine.Devices))
	}
	_, body = s.bearer(http.MethodGet, nativeapi.DevicesPath+"?include_revoked=true", phone.AccessToken)
	_ = json.Unmarshal(body, &mine)
	if len(mine.Devices) != 2 {
		t.Fatalf("include_revoked list = %d devices, want 2", len(mine.Devices))
	}
	// A user may revoke the device in use; its next call is refused.
	if status, _ := s.bearer(http.MethodDelete, nativeapi.DevicesPath+"/"+phone.DeviceID, phone.AccessToken); status != http.StatusNoContent {
		t.Fatalf("revoking the current device = %d", status)
	}
	if s.whoami(phone.AccessToken).Code != http.StatusUnauthorized {
		t.Fatal("the current device must be cut off once its owner revokes it")
	}

	// Administrators: filters, totals and revoked_by.
	admin := s.exchange(s.signIn(bob, "bob.devices@example.test"))
	status, body = s.bearer(http.MethodGet, "/api/v2/admin/native_devices/administration?state=all&user_id="+itoa(alice), admin.AccessToken)
	var all adminDeviceList
	if status != http.StatusOK || json.Unmarshal(body, &all) != nil || all.Total != 2 || len(all.Rows) != 2 ||
		all.Rows[0].Email != "alice.devices@example.test" {
		t.Fatalf("admin list (alice, all) = %d %s", status, body)
	}
	_, body = s.bearer(http.MethodGet, "/api/v2/admin/native_devices/administration?client_id="+testClientID, admin.AccessToken)
	var active adminDeviceList
	_ = json.Unmarshal(body, &active)
	if active.Total != 2 { // bob's two live devices
		t.Fatalf("admin list (active) total = %d, want 2: %s", active.Total, body)
	}
	if status, _ := s.bearer(http.MethodGet, "/api/v2/admin/native_devices/administration?state=bogus", admin.AccessToken); status != http.StatusBadRequest {
		t.Fatalf("bogus state = %d", status)
	}
	if status, _ := s.bearer(http.MethodDelete, "/api/v2/admin/native_devices/administration/"+bobs.DeviceID, admin.AccessToken); status != http.StatusNoContent {
		t.Fatalf("admin revoke = %d", status)
	}
	var revokedBy *int64
	_ = s.pool.QueryRow(context.Background(),
		`SELECT revoked_by FROM elitea_auth.native_sessions WHERE id = $1`, bobs.DeviceID).Scan(&revokedBy)
	if reason(s.family(bobs.DeviceID)) != domain.ReasonAdmin || revokedBy == nil || *revokedBy != bob {
		t.Fatalf("admin-revoked device = %+v revoked_by=%v", s.family(bobs.DeviceID), revokedBy)
	}
	if status, _ := s.bearer(http.MethodDelete, "/api/v2/admin/native_devices/administration/not-a-uuid", admin.AccessToken); status != http.StatusNotFound {
		t.Fatalf("admin revoke of a malformed id = %d", status)
	}
}

func TestNativeDeviceRoutesAnswer404WithNoClientRegistered(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("lonely@example.test")
	pair := s.exchange(s.signIn(userID, "lonely@example.test"))
	// The last client disappears from every layer.
	s.registry = domain.NewRegistry(nil, s.pool)
	empty := nativeapi.New(nativeapi.Config{Registry: s.registry, Store: s.store, PublicOrigin: testOrigin})
	recorder := doOnWithBearer(empty.RegisteredOnly(http.HandlerFunc(empty.ListDevices)), pair.AccessToken)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("devices with no client registered = %d, want 404", recorder.Code)
	}
}
