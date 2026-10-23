package repos

import (
	"errors"
	"testing"

	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

func TestCurrentIndexSchedulePresentPreservesCurrentMetadataShape(
	t *testing.T,
) {
	for name, test := range map[string]struct {
		raw     string
		index   string
		present bool
		wantErr error
	}{
		"entry present": {
			raw:   `{"indexes_meta":{"Docs":{"schedules":{"11":{}}}}}`,
			index: "Docs", present: true,
		},
		"entry absent": {
			raw:   `{"indexes_meta":{"Other":{}}}`,
			index: "Docs",
		},
		"indexes absent": {
			raw:   `{"other":true}`,
			index: "Docs",
		},
		"indexes null": {
			raw:     `{"indexes_meta":null}`,
			index:   "Docs",
			wantErr: indexmetaapp.ErrCurrentIndexScheduleUnavailable,
		},
		"null metadata": {
			raw:   `null`,
			index: "Docs",
		},
		"invalid indexes shape": {
			raw:     `{"indexes_meta":[]}`,
			index:   "Docs",
			wantErr: indexmetaapp.ErrCurrentIndexScheduleUnavailable,
		},
		"invalid json": {
			raw:     `{`,
			index:   "Docs",
			wantErr: indexmetaapp.ErrCurrentIndexScheduleUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			present, err := currentIndexSchedulePresent(
				[]byte(test.raw),
				test.index,
			)
			if present != test.present || !errors.Is(err, test.wantErr) {
				t.Fatalf(
					"present=%v want=%v err=%v want=%v",
					present,
					test.present,
					err,
					test.wantErr,
				)
			}
		})
	}
}
