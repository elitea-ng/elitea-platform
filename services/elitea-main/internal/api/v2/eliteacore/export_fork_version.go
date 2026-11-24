package eliteacore

import (
	"strconv"
	"strings"
)

// followedVersionIDs reads `follow_version_ids`: repeated values and
// comma-separated lists, numeric ids only. An empty or non-numeric value
// names nothing.
func followedVersionIDs(followVersionIDs []string) []string {
	var wanted []string
	for _, raw := range followVersionIDs {
		for _, part := range strings.Split(raw, ",") {
			if id := strings.TrimSpace(part); id != "" {
				if _, err := strconv.ParseInt(id, 10, 64); err == nil {
					wanted = append(wanted, id)
				}
			}
		}
	}
	return wanted
}

// forkVersions picks the one version a fork document carries (#1993).
//
// `?fork=true` keeps a single version. It used to keep the LAST one whatever
// the caller had open, so forking a named version from the agent page copied
// the newest version instead, and the copy's instructions and configuration
// did not match what the author had selected. The caller now names the version
// with `follow_version_ids` (the parameter the contract has always declared,
// and the old UI sent), and the matching version is the one kept.
//
// With no id named the newest version is kept, as before. An id that matches no
// version of the application is reported (found == false) rather than answered
// with a different version: a fork that silently copied something other than
// what was asked for is the bug this exists to close.
func forkVersions(versions []map[string]any, wanted []string) (kept []map[string]any, found bool) {
	if len(versions) == 0 {
		return versions, true
	}
	if len(wanted) == 0 {
		return versions[len(versions)-1:], true
	}
	for _, id := range wanted {
		for _, version := range versions {
			if version["id"] == id {
				return []map[string]any{version}, true
			}
		}
	}
	return nil, false
}

// namedVersions keeps every version a non-fork export names (issue 3010), in
// the application's version order. Names that match no version are reported
// (found == false) only when none matches: an export of the open version must
// not answer with a different one.
func namedVersions(versions []map[string]any, wanted []string) (kept []map[string]any, found bool) {
	named := map[string]bool{}
	for _, id := range wanted {
		named[id] = true
	}
	for _, version := range versions {
		if id, _ := version["id"].(string); named[id] {
			kept = append(kept, version)
		}
	}
	return kept, len(kept) > 0
}
