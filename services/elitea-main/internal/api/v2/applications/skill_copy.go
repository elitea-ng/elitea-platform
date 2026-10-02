package applications

import (
	"math"
	"strconv"
	"strings"
)

// An invalid optional source must not prevent version creation. Accept integer
// JSON numbers and decimal strings, but do not coerce booleans or fractions to IDs.
func optionalSkillSourceVersionID(raw any) int32 {
	switch value := raw.(type) {
	case float64:
		if value > 0 && value <= math.MaxInt32 && value == math.Trunc(value) {
			return int32(value)
		}
	case string:
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil && id > 0 {
			return int32(id)
		}
	}
	return 0
}
