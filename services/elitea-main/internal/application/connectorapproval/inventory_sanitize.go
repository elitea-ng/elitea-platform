package connectorapproval

import (
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
)

// SanitizeInventoryDraft makes an inventory draft (built from what the
// projects' toolkits hold, which no form validated) acceptable where it can,
// so one oddly configured connector does not block the on-ramp of every
// other one (#1537 review item 5):
//
//   - a tool whose name is not a tool name, or which repeats one, is dropped;
//   - a tool description that is not UTF-8 is repaired, and one longer than
//     MaxToolDescription is truncated, except a custom MCP server's: its
//     description is part of the pinned digest, so a truncated one would
//     pin a tool the server never serves, and the tool is dropped instead;
//   - a custom MCP server's tool with no stored input schema, or one larger
//     than MaxToolSchemaBytes, is dropped (it cannot be pinned);
//   - tools past MaxTools, invalid hosts and hosts past MaxEgressHosts are
//     dropped.
//
// Each adjustment is named in the justification, so the approver sees what
// the request does not cover. What is still invalid after this (a target
// that is not a target) is the caller's to skip and report.
func SanitizeInventoryDraft(draft Draft) Draft {
	var notes []string
	tools := make([]DeclaredTool, 0, len(draft.Tools))
	names := map[string]bool{}
	for _, tool := range draft.Tools {
		tool.Name = strings.TrimSpace(tool.Name)
		if !toolNamePattern.MatchString(tool.Name) {
			notes = append(notes, fmt.Sprintf("dropped the tool %q: not a tool name", truncate(tool.Name, 64)))
			continue
		}
		if names[strings.ToLower(tool.Name)] {
			continue
		}
		if tool.Category != "" && !tool.Category.Valid() {
			tool.Category = ""
		}
		tool.Description = strings.ToValidUTF8(tool.Description, "")
		if len(tool.Description) > MaxToolDescription {
			if draft.TargetKind == TargetCustomMCP {
				notes = append(notes, fmt.Sprintf("dropped the tool %q: its description is longer than %d bytes, so it cannot be pinned as served", tool.Name, MaxToolDescription))
				continue
			}
			tool.Description = strings.ToValidUTF8(tool.Description[:MaxToolDescription], "")
			notes = append(notes, fmt.Sprintf("truncated the description of the tool %q", tool.Name))
		}
		if draft.TargetKind == TargetCustomMCP {
			if tool.InputSchema == nil {
				notes = append(notes, fmt.Sprintf("dropped the tool %q: no stored input schema to pin (load the server's tools)", tool.Name))
				continue
			}
			canonical, err := CanonicalJSON(tool.InputSchema)
			if err != nil || len(canonical) > MaxToolSchemaBytes {
				notes = append(notes, fmt.Sprintf("dropped the tool %q: its input schema cannot be pinned", tool.Name))
				continue
			}
			if _, err := ToolDigest(tool.Name, tool.Description, tool.InputSchema); err != nil {
				notes = append(notes, fmt.Sprintf("dropped the tool %q: its definition cannot be pinned", tool.Name))
				continue
			}
		}
		if len(tools) == MaxTools {
			notes = append(notes, fmt.Sprintf("kept the first %d tools only", MaxTools))
			break
		}
		names[strings.ToLower(tool.Name)] = true
		tools = append(tools, tool)
	}
	draft.Tools = tools

	hosts := make([]string, 0, len(draft.EgressHosts))
	seen := map[string]bool{}
	for _, raw := range draft.EgressHosts {
		entry, err := egresslib.ParseEntry(strings.TrimSpace(raw))
		if err != nil {
			notes = append(notes, fmt.Sprintf("dropped the host %q: not an egress host", truncate(raw, 96)))
			continue
		}
		text := entry.String()
		if seen[text] {
			continue
		}
		if len(hosts) == MaxEgressHosts {
			notes = append(notes, fmt.Sprintf("kept the first %d hosts only", MaxEgressHosts))
			break
		}
		seen[text] = true
		hosts = append(hosts, text)
	}
	draft.EgressHosts = hosts

	if len(notes) > 0 {
		adjusted := strings.TrimSpace(draft.Justification) + " Adjusted from the toolkits' settings: " + strings.Join(notes, "; ") + "."
		if len(adjusted) > MaxJustification {
			adjusted = strings.ToValidUTF8(adjusted[:MaxJustification-3], "") + "..."
		}
		draft.Justification = adjusted
	}
	return draft
}
