package configurations

import (
	"reflect"
	"strings"
	"testing"
)

// TestUnpairedConfigurationFields pins the pairwise auth rule (F6). Legacy
// refuses a github credential that carries one half of username+password or
// app_id+app_private_key; the schema cannot state that, so the table does.
func TestUnpairedConfigurationFields(t *testing.T) {
	cases := []struct {
		name       string
		configType string
		data       map[string]any
		want       []string
	}{
		{"anonymous", "github", map[string]any{"base_url": "https://api.github.com"}, nil},
		{"token only", "github", map[string]any{"access_token": "{{secret.t}}"}, nil},
		{"username and password", "github", map[string]any{"username": "u", "password": "p"}, nil},
		{"app id and key", "github", map[string]any{"app_id": "1", "app_private_key": "k"}, nil},
		{"blank pair", "github", map[string]any{"username": "  ", "password": ""}, nil},
		{"null pair", "github", map[string]any{"app_id": nil, "app_private_key": nil}, nil},
		{"username without password", "github", map[string]any{"username": "u"}, []string{"data.username+data.password"}},
		{"password without username", "github", map[string]any{"password": "p", "username": " "}, []string{"data.username+data.password"}},
		{"app id without key", "github", map[string]any{"app_id": 12}, []string{"data.app_id+data.app_private_key"}},
		{"key without app id", "github", map[string]any{"app_private_key": "k"}, []string{"data.app_id+data.app_private_key"}},
		{
			"both pairs broken", "github",
			map[string]any{"username": "u", "app_private_key": "k"},
			[]string{"data.username+data.password", "data.app_id+data.app_private_key"},
		},
		{"type without a rule", "gitlab", map[string]any{"username": "u"}, nil},
		{"no data", "github", nil, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := unpairedConfigurationFields(testCase.configType, testCase.data)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("unpaired = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestPairedFieldsExistInThePinnedSchema keeps the table honest: every field
// it names is a property of the type's own data schema in the snapshot.
func TestPairedFieldsExistInThePinnedSchema(t *testing.T) {
	for configType, pairs := range configurationPairedFields {
		schema := string(pinnedSchemaFor(t, configType))
		for _, pair := range pairs {
			for _, field := range []string{pair.first, pair.second} {
				if !containsQuoted(schema, field) {
					t.Errorf("%s: the pinned schema names no %q property", configType, field)
				}
			}
		}
	}
}

func containsQuoted(haystack, field string) bool {
	return strings.Contains(haystack, `"`+field+`"`)
}
