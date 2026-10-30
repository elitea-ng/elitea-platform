package platformconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestEntitlementExportSettingsFromValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		values Values
		want   []int64
	}{
		{name: "never saved", values: Values{}, want: []int64{}},
		{name: "empty", values: Values{"scim_client_ids": []any{}}, want: []int64{}},
		{name: "listed", values: Values{"scim_client_ids": []any{float64(3), float64(9)}}, want: []int64{3, 9}},
		{name: "malformed entries narrow", values: Values{"scim_client_ids": []any{"3", float64(0), float64(-2), 1.5, nil, float64(4)}},
			want: []int64{4}},
		{name: "not a list", values: Values{"scim_client_ids": "3"}, want: []int64{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := EntitlementExportSettingsFromValues(test.values)
			if !reflect.DeepEqual(got.SCIMClientIDs, test.want) {
				t.Fatalf("got %v, want %v", got.SCIMClientIDs, test.want)
			}
		})
	}
	if DefaultEntitlementExportSettings().AllowsSCIMClient(1) {
		t.Fatal("the default lists a client")
	}
	if !(EntitlementExportSettings{SCIMClientIDs: []int64{2, 5}}).AllowsSCIMClient(5) {
		t.Fatal("a listed client is refused")
	}
}

func TestValidateEntitlementExportValues(t *testing.T) {
	many := make([]any, MaxEntitlementExportSCIMClients+1)
	for index := range many {
		many[index] = float64(index + 1)
	}
	for _, test := range []struct {
		name   string
		values Values
		want   string
	}{
		{name: "absent", values: Values{}},
		{name: "empty", values: Values{"scim_client_ids": []any{}}},
		{name: "valid", values: Values{"scim_client_ids": []any{float64(1), float64(12)}}},
		{name: "not an array", values: Values{"scim_client_ids": float64(1)}, want: "must be an array"},
		{name: "a string", values: Values{"scim_client_ids": []any{"1"}}, want: `[0] must be a positive integer`},
		{name: "zero", values: Values{"scim_client_ids": []any{float64(1), float64(0)}}, want: `[1] must be a positive integer`},
		{name: "fraction", values: Values{"scim_client_ids": []any{1.5}}, want: "positive integer"},
		{name: "too many", values: Values{"scim_client_ids": many}, want: "at most 64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateEntitlementExportValues(test.values)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
