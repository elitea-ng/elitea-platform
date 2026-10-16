package workspacehandoff

import (
	"encoding/json"
	"testing"
)

func TestEventMessages(t *testing.T) {
	stats, _ := json.Marshal(Stats{Files: 42, Excluded: []string{"./.env", "id_rsa", "x.pem"}})
	if got := CompleteMessage(stats, ""); got != "Work moved to the cloud executor (42 files, 3 left on the laptop)." {
		t.Fatalf("complete message = %q", got)
	}
	if got := CompleteMessage(stats, "  Changed parser.go; ran go test.  "); got !=
		"Work moved to the cloud executor (42 files, 3 left on the laptop).\n\nChanged parser.go; ran go test." {
		t.Fatalf("complete message with a note = %q", got)
	}
	if got := ReleaseMessage("Alex's MacBook"); got != "Work returned to Alex's MacBook." {
		t.Fatalf("release message = %q", got)
	}
	if got := ReleaseMessage(" "); got != "Work returned to the desktop." {
		t.Fatalf("release message without a device = %q", got)
	}
}

func TestCloudTurnMetaKeys(t *testing.T) {
	meta := CloudTurnMeta(2, "0123456789abcdef0123456789abcdef01234567")
	if meta["executed_by"] != "cloud_executor" || meta["workspace_epoch"] != int64(2) ||
		meta["workspace_commit"] != "0123456789abcdef0123456789abcdef01234567" || len(meta) != 3 {
		t.Fatalf("meta = %v", meta)
	}
	if _, present := CloudTurnMeta(2, "")["workspace_commit"]; present {
		t.Fatalf("an unpublished turn names a commit")
	}
}
