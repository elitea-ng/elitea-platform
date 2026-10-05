package repos

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The gateway's request-log retention is a compiled constant in a module
// outside go.work, so it cannot be imported. requestLogRetentionSQL mirrors
// it, and this test reads the gateway source so the two cannot drift apart:
// a shorter gateway window would make the run reads report pruned figures as
// complete.
func TestRequestLogRetentionMatchesTheGateway(t *testing.T) {
	source := filepath.Join("..", "..", "..", "..", "..", "elitea-llm-gateway", "internal", "requestlog", "requestlog.go")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the gateway retention constant: %v", err)
	}
	gateway := regexp.MustCompile(`RetentionWindow\s*=\s*(\d+)\s*\*\s*24\s*\*\s*time\.Hour`).FindSubmatch(body)
	if gateway == nil {
		t.Fatalf("%s no longer declares RetentionWindow as `<days> * 24 * time.Hour`; update requestLogRetentionSQL and this test", source)
	}
	mirror := regexp.MustCompile(`^interval '(\d+) days'$`).FindStringSubmatch(requestLogRetentionSQL)
	if mirror == nil {
		t.Fatalf("requestLogRetentionSQL = %q, want interval '<days> days'", requestLogRetentionSQL)
	}
	gatewayDays, _ := strconv.Atoi(string(gateway[1]))
	mirrorDays, _ := strconv.Atoi(mirror[1])
	if gatewayDays != mirrorDays {
		t.Fatalf("gateway keeps %d days of request log, the run reads assume %d", gatewayDays, mirrorDays)
	}
}
