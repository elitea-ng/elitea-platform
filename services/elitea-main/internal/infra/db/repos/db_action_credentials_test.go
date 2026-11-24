package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
)

type spyDBUnsecreter struct {
	seenKeys []string
	out      map[string]any
	err      error
}

func (s *spyDBUnsecreter) Unsecret(_ context.Context, _ int32, data map[string]any) (map[string]any, error) {
	for k := range data {
		s.seenKeys = append(s.seenKeys, k)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.out != nil {
		return s.out, nil
	}
	return map[string]any{"password": "resolved-password"}, nil
}

func TestDBActionCredentialsMaterializeJSONRoundTripAndHardening(t *testing.T) {
	t.Parallel()

	frozen := map[string]any{
		configurationapp.CurrentFrozenConfigurationMarker: true,
		"configuration_uuid":                              "60000000-0000-4000-8000-000000000721",
		"configuration_project_id":                        int32(1),
		"configuration_type":                              "sql",
		"host":                                            "warehouse.example.com",
		"port":                                            5432,
		"username":                                        "etl_ro",
		"password":                                        "{{secret.WAREHOUSE_PASS}}",
	}
	rawJSON, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Standard json.Unmarshal (float64 numbers) succeeds and unsecrets ONLY password.
	var unmarshaledFloat map[string]any
	if err := json.Unmarshal(rawJSON, &unmarshaledFloat); err != nil {
		t.Fatal(err)
	}
	spy := &spyDBUnsecreter{}
	creds := &DBActionCredentials{unsecreter: spy}
	mat, err := creds.MaterializeSQLCredential(context.Background(), 1, dbaction.DialectPostgres, unmarshaledFloat)
	if err != nil {
		t.Fatalf("MaterializeSQLCredential(float64 JSON): %v", err)
	}
	if mat.Host != "warehouse.example.com" || mat.Port != 5432 || mat.Username != "etl_ro" || mat.Password() != "resolved-password" {
		t.Fatalf("unexpected materialized credential: host=%s port=%d user=%s", mat.Host, mat.Port, mat.Username)
	}
	if len(spy.seenKeys) != 1 || spy.seenKeys[0] != "password" {
		t.Fatalf("unsecreter received keys %v, want strictly [password]", spy.seenKeys)
	}
	mat.Zero()

	// 2. json.Decoder with UseNumber() (json.Number) succeeds.
	var unmarshaledNumber map[string]any
	dec := json.NewDecoder(bytes.NewReader(rawJSON))
	dec.UseNumber()
	if err := dec.Decode(&unmarshaledNumber); err != nil {
		t.Fatal(err)
	}
	matNum, err := creds.MaterializeSQLCredential(context.Background(), 1, dbaction.DialectPostgres, unmarshaledNumber)
	if err != nil {
		t.Fatalf("MaterializeSQLCredential(json.Number): %v", err)
	}
	matNum.Zero()

	// 3. Canceled and deadline-exceeded contexts are preserved (not collapsed into ErrUnavailable).
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := creds.MaterializeSQLCredential(canceledCtx, 1, dbaction.DialectPostgres, unmarshaledFloat); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx err = %v, want context.Canceled", err)
	}
	deadlineCreds := &DBActionCredentials{unsecreter: &spyDBUnsecreter{err: context.DeadlineExceeded}}
	if _, err := deadlineCreds.MaterializeSQLCredential(context.Background(), 1, dbaction.DialectPostgres, unmarshaledFloat); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err = %v, want context.DeadlineExceeded", err)
	}

	// 4. Negative hardening cases: unsealed plaintext password, secret placeholder in username,
	// missing configuration_uuid, extra/nested key, non-integral float64 project_id.
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unsealed plaintext password", func(m map[string]any) { m["password"] = "raw-plaintext" }},
		{"secret placeholder in username", func(m map[string]any) { m["username"] = "{{secret.ADMIN}}" }},
		{"missing configuration_uuid", func(m map[string]any) { delete(m, "configuration_uuid") }},
		{"extra nested foreign config key", func(m map[string]any) {
			m["nested"] = map[string]any{
				configurationapp.CurrentFrozenConfigurationMarker: true,
				"configuration_project_id":                        2,
			}
		}},
		{"fractional project_id", func(m map[string]any) { m["configuration_project_id"] = 1.5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyMap := make(map[string]any, len(unmarshaledFloat))
			for k, v := range unmarshaledFloat {
				copyMap[k] = v
			}
			tc.mutate(copyMap)
			if _, err := creds.MaterializeSQLCredential(context.Background(), 1, dbaction.DialectPostgres, copyMap); !errors.Is(err, dbaction.ErrPolicyDenied) {
				t.Fatalf("%s: err = %v, want ErrPolicyDenied", tc.name, err)
			}
		})
	}
}
