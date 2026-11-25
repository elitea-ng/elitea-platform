package adminaccess_test

import (
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// TestConsoleItemTable holds the item table of decision 6 to the permission
// table: every item maps at least one registered name and nothing else, ids
// are unique, every area has an item and none has more than eight.
func TestConsoleItemTable(t *testing.T) {
	t.Parallel()
	perArea := map[adminaccess.Area]int{}
	seen := map[string]bool{}
	for _, item := range adminaccess.ConsoleItems() {
		if item.ID == "" || seen[item.ID] {
			t.Errorf("item id %q is empty or appears twice", item.ID)
		}
		seen[item.ID] = true
		if !slices.Contains(adminaccess.Areas(), item.Area) {
			t.Errorf("item %q is in the unknown area %q", item.ID, item.Area)
		}
		perArea[item.Area]++
		if len(item.Permissions) == 0 {
			t.Errorf("item %q maps no permission", item.ID)
		}
		for _, name := range item.Permissions {
			if _, ok := adminaccess.Lookup(name); !ok {
				t.Errorf("item %q maps %q, which is not a row of the permission table", item.ID, name)
			}
		}
	}
	for _, area := range adminaccess.Areas() {
		switch n := perArea[area]; {
		case n == 0:
			t.Errorf("area %q has no item", area)
		case n > adminaccess.MaxConsoleAreaItems:
			t.Errorf("area %q has %d items, more than %d", area, n, adminaccess.MaxConsoleAreaItems)
		}
	}
}

// TestConsoleItemsWithNoReadHalf: the items decision 2 says have no read
// half map no read name, so a caller sees them at write or not at all.
func TestConsoleItemsWithNoReadHalf(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"data-retention", "branding", "kill-switch", "toolkits", "client-applications"} {
		item, ok := adminaccess.LookupConsoleItem(id)
		if !ok {
			t.Fatalf("no item %q", id)
		}
		for _, name := range item.Permissions {
			if row, _ := adminaccess.Lookup(name); row.Class == adminaccess.ClassRead {
				t.Errorf("item %q maps the read name %q; it has no read half", id, name)
			}
		}
	}
}

// TestConsoleItemsThatShipLater are in the table and out of the document.
func TestConsoleItemsThatShipLater(t *testing.T) {
	t.Parallel()
	var unavailable []string
	for _, item := range adminaccess.ConsoleItems() {
		if !item.Available {
			unavailable = append(unavailable, item.ID)
		}
	}
	want := []string{"project-settings", "security-signals", "access-history", "break-glass"}
	if !slices.Equal(unavailable, want) {
		t.Errorf("unavailable items: got %q, want %q", unavailable, want)
	}
	every := adminaccess.SetOf(adminaccess.Names()...)
	for _, area := range adminaccess.ConsoleAccess(every) {
		for _, access := range area.Items {
			if slices.Contains(want, access.Item.ID) {
				t.Errorf("the unavailable item %q is in the document", access.Item.ID)
			}
		}
	}
}

func TestConsoleItemLevel(t *testing.T) {
	t.Parallel()
	secrets, _ := adminaccess.LookupConsoleItem("secrets")
	for _, tc := range []struct {
		held  []string
		level adminaccess.ConsoleLevel
		open  bool
	}{
		{nil, "", false},
		{[]string{"fixture.not.registered"}, "", false},
		{[]string{adminaccess.ConfigurationSecretsSecretView}, adminaccess.ConsoleRead, true},
		{[]string{adminaccess.ConfigurationSecretsSecretView, adminaccess.ConfigurationSecretsSecretEdit},
			adminaccess.ConsoleWrite, true},
		// A name of another item opens nothing here.
		{[]string{adminaccess.ConfigurationBranding}, "", false},
	} {
		level, open := secrets.Level(adminaccess.SetOf(tc.held...))
		if level != tc.level || open != tc.open {
			t.Errorf("held %q: got %q %v, want %q %v", tc.held, level, open, tc.level, tc.open)
		}
	}
	// approve counts as write.
	connectors, _ := adminaccess.LookupConsoleItem("connector-requests")
	if level, _ := connectors.Level(adminaccess.SetOf(adminaccess.ConnectorApprove)); level != adminaccess.ConsoleWrite {
		t.Errorf("connector.approve: got %q, want write", level)
	}
}

func TestConsoleAccessIsEmptyWithNoAdministrationPermission(t *testing.T) {
	t.Parallel()
	if areas := adminaccess.ConsoleAccess(adminaccess.SetOf("models.chat.conversation.details")); len(areas) != 0 {
		t.Errorf("got %d areas for a caller with no administration permission, want 0", len(areas))
	}
}
