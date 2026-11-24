package secretstrength

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestChartRefusesTheSameBurnedValues keeps the Helm chart's render-time
// check (deploy/helm/elitea/templates/_secret-strength.tpl) on the same
// burned list as the start-up check. A digest added here and not there would
// let `helm template` accept a value the service then refuses.
func TestChartRefusesTheSameBurnedValues(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "helm", "elitea", "templates", "_secret-strength.tpl")
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the chart's check: %v", err)
	}
	chart := map[string]struct{}{}
	for _, d := range regexp.MustCompile(`(?m)^[0-9a-f]{64}$`).FindAllString(string(text), -1) {
		chart[d] = struct{}{}
	}
	for d := range burnedDigests {
		if _, ok := chart[d]; !ok {
			t.Errorf("the chart does not refuse burned digest %s", d)
		}
	}
	for d := range chart {
		if _, ok := burnedDigests[d]; !ok {
			t.Errorf("the chart refuses digest %s, which this package does not", d)
		}
	}
}
