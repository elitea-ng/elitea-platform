package eliteacore

import "testing"

func TestForkVersionsKeepsTheNamedVersion(t *testing.T) {
	versions := []map[string]any{{"id": "1"}, {"id": "2"}, {"id": "3"}}

	kept, found := forkVersions(versions, []string{"2"})
	if !found || len(kept) != 1 || kept[0]["id"] != "2" {
		t.Fatalf("named version 2: kept=%v found=%v", kept, found)
	}

	kept, found = forkVersions(versions, nil)
	if !found || len(kept) != 1 || kept[0]["id"] != "3" {
		t.Fatalf("no id: want the newest, kept=%v found=%v", kept, found)
	}

	if _, found = forkVersions(versions, []string{"99"}); found {
		t.Fatal("an id that matches no version must be reported, not answered with another version")
	}
}
