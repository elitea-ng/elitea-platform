package eliteacore

import "testing"

func TestForkVersionsKeepsTheNamedVersion(t *testing.T) {
	versions := []map[string]any{{"id": "1"}, {"id": "2"}, {"id": "3"}}

	kept, found := forkVersions(versions, followedVersionIDs([]string{"2"}))
	if !found || len(kept) != 1 || kept[0]["id"] != "2" {
		t.Fatalf("named version 2: kept=%v found=%v", kept, found)
	}

	kept, found = forkVersions(versions, nil)
	if !found || len(kept) != 1 || kept[0]["id"] != "3" {
		t.Fatalf("no id: want the newest, kept=%v found=%v", kept, found)
	}

	if _, found = forkVersions(versions, followedVersionIDs([]string{"99"})); found {
		t.Fatal("an id that matches no version must be reported, not answered with another version")
	}
}

func TestFollowedVersionIDsNamesOnlyNumericIDs(t *testing.T) {
	if got := followedVersionIDs([]string{"", ",", "abc"}); len(got) != 0 {
		t.Fatalf("an empty or non-numeric value names nothing, got %v", got)
	}
	if got := followedVersionIDs([]string{"3, 1", "2"}); len(got) != 3 {
		t.Fatalf("lists and repeats: got %v", got)
	}
}

// A non-fork export keeps every version it names, in version order (issue 3010).
func TestNamedVersionsKeepsEveryNamedVersion(t *testing.T) {
	versions := []map[string]any{{"id": "1"}, {"id": "2"}, {"id": "3"}}
	kept, found := namedVersions(versions, []string{"3", "1", "99"})
	if !found || len(kept) != 2 || kept[0]["id"] != "1" || kept[1]["id"] != "3" {
		t.Fatalf("named 3 and 1: kept=%v found=%v", kept, found)
	}
	if _, found := namedVersions(versions, []string{"99"}); found {
		t.Fatal("names that match no version must be reported")
	}
}
