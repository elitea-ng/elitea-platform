package configurations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	currentSecretIDOne = "11111111111111111111111111111111"
	currentSecretIDTwo = "22222222222222222222222222222222"
)

func TestExtractCurrentConfigurationSecretsPreservesCurrentFieldBehavior(t *testing.T) {
	data := map[string]any{
		"api_key":   "raw-api-key",
		"empty":     "",
		"name":      "connection",
		"nothing":   nil,
		"reference": "{{secret.Existing_1}}",
	}
	properties := map[string]any{
		"api_key": map[string]any{"format": "password"},
		"empty": map[string]any{
			"anyOf": []any{
				map[string]any{"type": "string", "format": "password"},
				map[string]any{"type": "null"},
			},
		},
		"name":    map[string]any{"type": "string"},
		"nothing": map[string]any{"format": "password"},
		"reference": map[string]any{
			"anyOf": []map[string]any{
				map[string]any{"format": "password"},
				map[string]any{"type": "null"},
			},
		},
	}

	ids, calls := currentSecretIDs(currentSecretIDOne)
	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
		context.Background(), data, properties, "github", ids,
	)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("ID generator calls = %d, want 1", *calls)
	}
	if sanitized["api_key"] != "{{secret."+currentSecretIDOne+"}}" {
		t.Fatalf("password was not sanitized: %#v", sanitized["api_key"])
	}
	if sanitized["empty"] != "" || sanitized["nothing"] != nil {
		t.Fatalf("empty/null values changed: %#v", sanitized)
	}
	if sanitized["reference"] != "{{secret.Existing_1}}" || sanitized["name"] != "connection" {
		t.Fatalf("non-raw values changed: %#v", sanitized)
	}
	if len(mutations) != 1 || mutations[0].Field != "api_key" ||
		!reflect.DeepEqual(mutations[0].Path, []string{"api_key"}) ||
		mutations[0].Name != currentSecretIDOne || mutations[0].Value != "raw-api-key" {
		t.Fatalf("unexpected mutation metadata: name=%q count=%d", mutationName(mutations), len(mutations))
	}
	if data["api_key"] != "raw-api-key" {
		t.Fatal("caller data was modified")
	}
}

func TestExtractCurrentConfigurationSecretsRequiresExactReferenceSyntax(t *testing.T) {
	malformed := []string{
		"{{secret.}}",
		"{{secret.bad-name}}",
		"prefix{{secret.name}}",
		"{{secret.name}}suffix",
	}
	for _, value := range malformed {
		t.Run(value, func(t *testing.T) {
			sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
				context.Background(),
				map[string]any{"token": value},
				map[string]any{"token": map[string]any{"format": "password"}},
				"test",
				func() (string, error) { return currentSecretIDOne, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if sanitized["token"] != "{{secret."+currentSecretIDOne+"}}" {
				t.Fatalf("malformed reference was not sanitized: %#v", sanitized["token"])
			}
			if len(mutations) != 1 || mutations[0].Value != value {
				t.Fatal("malformed reference was not treated as a raw secret")
			}
		})
	}
}

func TestExtractCurrentConfigurationSecretsRejectsUnknownTopLevelFieldFirst(t *testing.T) {
	data := map[string]any{
		"aaa_password": "must-not-appear",
		"zzz_unknown":  "also-must-not-appear",
	}
	properties := map[string]any{
		"aaa_password": map[string]any{"format": "password"},
	}
	called := false

	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
		context.Background(), data, properties, "custom", func() (string, error) {
			called = true
			return currentSecretIDOne, nil
		},
	)
	if sanitized != nil || mutations != nil {
		t.Fatal("failed extraction returned partial output")
	}
	if called {
		t.Fatal("unknown-field validation allocated a secret ID")
	}
	if !errors.Is(err, ErrInvalidCurrentConfigurationSecrets) {
		t.Fatalf("error identity = %v", err)
	}
	var fieldError *CurrentSecretFieldError
	if !errors.As(err, &fieldError) || fieldError.Field != "zzz_unknown" {
		t.Fatalf("field error = %#v", fieldError)
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Fatalf("error exposed configuration values: %v", err)
	}
	if data["aaa_password"] != "must-not-appear" {
		t.Fatal("caller data changed after validation failure")
	}
}

func TestExtractCurrentConfigurationSecretsRecursesThroughNestedObjectSchemas(t *testing.T) {
	data := map[string]any{
		"connection": map[string]any{
			"auth": map[string]any{
				"note":  "forward-compatible",
				"token": "nested-token",
			},
			"password": "nested-password",
		},
		"optional": map[string]any{
			"client_secret": "optional-secret",
		},
	}
	properties := map[string]any{
		"connection": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"auth": map[string]any{
					"properties": map[string]any{
						"token": map[string]any{"format": "password"},
					},
				},
				"password": map[string]any{"format": "password"},
			},
		},
		"optional": map[string]any{
			"anyOf": []any{
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"client_secret": map[string]any{
							"anyOf": []any{
								map[string]any{"format": "password"},
								map[string]any{"type": "null"},
							},
						},
					},
				},
				map[string]any{"type": "null"},
			},
		},
	}

	ids, _ := currentSecretIDs(
		currentSecretIDOne,
		currentSecretIDTwo,
		"33333333333333333333333333333333",
	)
	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
		context.Background(), data, properties, "nested", ids,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 3 {
		t.Fatalf("mutation count = %d, want 3", len(mutations))
	}
	// Sorted field paths make ID assignment stable: connection.auth.token,
	// connection.password, then optional.client_secret.
	if mutations[0].Field != "connection.auth.token" ||
		!reflect.DeepEqual(mutations[0].Path, []string{"connection", "auth", "token"}) ||
		mutations[0].Value != "nested-token" ||
		mutations[1].Field != "connection.password" ||
		!reflect.DeepEqual(mutations[1].Path, []string{"connection", "password"}) ||
		mutations[1].Value != "nested-password" ||
		mutations[2].Field != "optional.client_secret" ||
		!reflect.DeepEqual(mutations[2].Path, []string{"optional", "client_secret"}) ||
		mutations[2].Value != "optional-secret" {
		t.Fatal("mutations are not in deterministic field-path order")
	}

	connection := sanitized["connection"].(map[string]any)
	auth := connection["auth"].(map[string]any)
	if auth["token"] != "{{secret."+currentSecretIDOne+"}}" || auth["note"] != "forward-compatible" {
		t.Fatalf("nested object was not sanitized: %#v", auth)
	}
	if connection["password"] != "{{secret."+currentSecretIDTwo+"}}" {
		t.Fatalf("nested password was not sanitized: %#v", connection["password"])
	}
	if data["connection"].(map[string]any)["password"] != "nested-password" {
		t.Fatal("nested caller data was modified")
	}
	auth["note"] = "changed"
	if data["connection"].(map[string]any)["auth"].(map[string]any)["note"] != "forward-compatible" {
		t.Fatal("nested output aliases caller data")
	}
}

func TestExtractCurrentConfigurationSecretsRejectsIdentifierCollisions(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string]any
		properties map[string]any
		ids        []string
		wantField  string
	}{
		{
			name: "two generated identifiers",
			data: map[string]any{"a": "first-secret", "b": "second-secret"},
			properties: map[string]any{
				"a": map[string]any{"format": "password"},
				"b": map[string]any{"format": "password"},
			},
			ids:       []string{currentSecretIDOne, currentSecretIDOne},
			wantField: "b",
		},
		{
			name: "generated identifier and later existing reference",
			data: map[string]any{
				"a": "first-secret",
				"z": "{{secret." + currentSecretIDOne + "}}",
			},
			properties: map[string]any{
				"a": map[string]any{"format": "password"},
				"z": map[string]any{"format": "password"},
			},
			ids:       []string{currentSecretIDOne},
			wantField: "z",
		},
		{
			name: "existing reference and later generated identifier",
			data: map[string]any{
				"a": "{{secret." + currentSecretIDOne + "}}",
				"z": "second-secret",
			},
			properties: map[string]any{
				"a": map[string]any{"format": "password"},
				"z": map[string]any{"format": "password"},
			},
			ids:       []string{currentSecretIDOne},
			wantField: "z",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ids, _ := currentSecretIDs(test.ids...)
			sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
				context.Background(), test.data, test.properties, "test", ids,
			)
			if sanitized != nil || mutations != nil {
				t.Fatal("collision returned partial output")
			}
			var fieldError *CurrentSecretFieldError
			if !errors.As(err, &fieldError) || fieldError.Field != test.wantField {
				t.Fatalf("field error = %#v, want %q", fieldError, test.wantField)
			}
			for _, value := range []string{"first-secret", "second-secret"} {
				if strings.Contains(err.Error(), value) {
					t.Fatal("collision error exposed a plaintext value")
				}
			}
		})
	}

	shared := "{{secret.shared_name}}"
	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
		context.Background(),
		map[string]any{"a": shared, "b": shared},
		map[string]any{
			"a": map[string]any{"format": "password"},
			"b": map[string]any{"format": "password"},
		},
		"test",
		nil,
	)
	if err != nil || len(mutations) != 0 || sanitized["a"] != shared || sanitized["b"] != shared {
		t.Fatalf("reused existing reference was rejected: sanitized=%#v mutations=%d err=%v", sanitized, len(mutations), err)
	}
}

func TestExtractCurrentConfigurationSecretsSanitizesGeneratorFailures(t *testing.T) {
	tests := []struct {
		name      string
		generator CurrentSecretIDGenerator
	}{
		{name: "missing generator"},
		{name: "generator error", generator: func() (string, error) {
			return "", errors.New("generator-sensitive-detail")
		}},
		{name: "short identifier", generator: func() (string, error) { return "abc", nil }},
		{name: "uppercase identifier", generator: func() (string, error) {
			return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil
		}},
		{name: "non hexadecimal identifier", generator: func() (string, error) {
			return "gggggggggggggggggggggggggggggggg", nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ExtractCurrentConfigurationSecrets(
				context.Background(),
				map[string]any{"token": "plaintext-sensitive-detail"},
				map[string]any{"token": map[string]any{"format": "password"}},
				"test",
				test.generator,
			)
			if !errors.Is(err, ErrInvalidCurrentConfigurationSecrets) {
				t.Fatalf("error identity = %v", err)
			}
			if strings.Contains(err.Error(), "sensitive-detail") {
				t.Fatalf("error exposed a sensitive value: %v", err)
			}
		})
	}
}

func TestExtractCurrentConfigurationSecretsRejectsNonStringPasswordWithoutValue(t *testing.T) {
	values := []any{42, true, map[string]any{"value": "not-exposed"}, []any{"not-exposed"}}
	for _, value := range values {
		_, _, err := ExtractCurrentConfigurationSecrets(
			context.Background(),
			map[string]any{"token": value},
			map[string]any{"token": map[string]any{"format": "password"}},
			"test",
			nil,
		)
		if !errors.Is(err, ErrInvalidCurrentConfigurationSecrets) {
			t.Fatalf("value type %T error = %v", value, err)
		}
		if strings.Contains(err.Error(), "not-exposed") || strings.Contains(err.Error(), "42") {
			t.Fatalf("error exposed value for type %T: %v", value, err)
		}
	}
}

func TestExtractCurrentConfigurationSecretsPreservesCancellationIdentity(t *testing.T) {
	properties := map[string]any{"token": map[string]any{"format": "password"}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, _, err := ExtractCurrentConfigurationSecrets(
		canceled, map[string]any{"token": "value"}, properties, "test", func() (string, error) {
			called = true
			return currentSecretIDOne, nil
		},
	)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("pre-canceled result: called=%v err=%v", called, err)
	}

	duringGeneration, cancelDuringGeneration := context.WithCancel(context.Background())
	_, _, err = ExtractCurrentConfigurationSecrets(
		duringGeneration, map[string]any{"token": "value"}, properties, "test", func() (string, error) {
			cancelDuringGeneration()
			return currentSecretIDOne, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after generation = %v", err)
	}

	var nilContext context.Context
	_, _, err = ExtractCurrentConfigurationSecrets(nilContext, map[string]any{}, map[string]any{}, "test", nil)
	if !errors.Is(err, ErrInvalidCurrentConfigurationSecrets) {
		t.Fatalf("nil context error = %v", err)
	}
}

func TestExtractCurrentConfigurationSecretsPreservesNilEmptyAndCopiesJSON(t *testing.T) {
	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(
		context.Background(), nil, nil, "test", nil,
	)
	if err != nil || sanitized != nil || mutations != nil {
		t.Fatalf("nil result: sanitized=%#v mutations=%#v err=%v", sanitized, mutations, err)
	}

	sanitized, mutations, err = ExtractCurrentConfigurationSecrets(
		context.Background(), map[string]any{}, map[string]any{}, "test", nil,
	)
	if err != nil || sanitized == nil || len(sanitized) != 0 || mutations != nil {
		t.Fatalf("empty result: sanitized=%#v mutations=%#v err=%v", sanitized, mutations, err)
	}

	data := map[string]any{
		"metadata": map[string]any{
			"items": []any{map[string]any{"name": "first"}},
		},
	}
	sanitized, _, err = ExtractCurrentConfigurationSecrets(
		context.Background(),
		data,
		map[string]any{"metadata": map[string]any{"type": "object"}},
		"test",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	items := sanitized["metadata"].(map[string]any)["items"].([]any)
	items[0].(map[string]any)["name"] = "changed"
	want := map[string]any{"metadata": map[string]any{"items": []any{map[string]any{"name": "first"}}}}
	if !reflect.DeepEqual(data, want) {
		t.Fatal("non-secret JSON output aliases caller data")
	}
}

func currentSecretIDs(values ...string) (CurrentSecretIDGenerator, *int) {
	index := 0
	return func() (string, error) {
		if index >= len(values) {
			return "", errors.New("unexpected ID allocation")
		}
		value := values[index]
		index++
		return value, nil
	}, &index
}

func mutationName(mutations []HiddenSecretMutation) string {
	if len(mutations) == 0 {
		return ""
	}
	return mutations[0].Name
}

// #1204: only whole-string references with the minted 32-hex shape name a
// hidden secret a row owns. A user-named secret, an embedded reference and a
// malformed one are never reported, so a delete never purges them.
func TestCurrentHiddenSecretReferencesNamesOnlyMintedReferences(t *testing.T) {
	data := map[string]any{
		"api_key":  "{{secret." + currentSecretIDTwo + "}}",
		"named":    "{{secret.my_github_token}}",
		"embedded": "Bearer {{secret." + currentSecretIDOne + "}}",
		"upper":    "{{secret.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA}}",
		"nested": map[string]any{
			"list": []any{"{{secret." + currentSecretIDOne + "}}", "{{secret." + currentSecretIDTwo + "}}"},
		},
	}
	got := CurrentHiddenSecretReferences(data)
	want := []string{currentSecretIDOne, currentSecretIDTwo}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CurrentHiddenSecretReferences = %v, want %v", got, want)
	}
}
