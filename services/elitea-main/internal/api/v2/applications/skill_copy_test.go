package applications

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func TestOptionalSkillSourceVersionID(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  any
		want int32
	}{
		{"number", float64(42), 42}, {"string", "42", 42},
		{"whitespace", " 42 ", 42}, {"maximum", float64(math.MaxInt32), math.MaxInt32},
		{"missing", nil, 0}, {"zero", float64(0), 0}, {"negative", float64(-1), 0},
		{"overflow", float64(math.MaxInt32) + 1, 0}, {"large string", "2147483648", 0},
		{"boolean", true, 0}, {"fraction", 1.5, 0}, {"decimal string", "1.5", 0},
		{"object", map[string]any{"id": 1}, 0}, {"array", []any{1}, 0},
		{"empty", "", 0}, {"malformed", "1; DELETE FROM applications", 0},
		{"nan", math.NaN(), 0}, {"infinity", math.Inf(1), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := optionalSkillSourceVersionID(test.raw); got != test.want {
				t.Fatalf("source ID = %d, want %d", got, test.want)
			}
		})
	}
}

func FuzzOptionalSkillSourceVersionID(f *testing.F) {
	for _, seed := range []string{`42`, `"42"`, `null`, `true`, `1.5`, `2147483648`, `{}`, `[]`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		var value any
		if json.Unmarshal([]byte(raw), &value) != nil {
			return
		}
		id := optionalSkillSourceVersionID(value)
		if id < 0 {
			t.Fatal("invalid optional input produced a negative ID")
		}
		if id > 0 && (optionalSkillSourceVersionID(float64(id)) != id ||
			optionalSkillSourceVersionID(strconv.FormatInt(int64(id), 10)) != id) {
			t.Fatal("canonical numeric and string IDs disagree")
		}
	})
}
