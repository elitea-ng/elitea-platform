package toolkits_test

import (
	"sort"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// The classification rule, stated as the cases state it, one example per
// decision the rule has to get right. The pairs matter more than the
// individual answers: `search_index`/`index_data` and `list_indexes`/
// `remove_index` are the names a last-token or any-token rule gets backwards.
func TestClassifyToolGroup(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]toolkits.ToolGroup{
		// Read — the fallback and the majority.
		"get_issue":                toolkits.ToolGroupRead,
		"list_projects":            toolkits.ToolGroupRead,
		"read_multiple_files":      toolkits.ToolGroupRead,
		"search_using_jql":         toolkits.ToolGroupRead,
		"grep_file":                toolkits.ToolGroupRead,
		"stepback_search_index":    toolkits.ToolGroupRead,
		"search_index":             toolkits.ToolGroupRead,
		"list_indexes":             toolkits.ToolGroupRead,
		"searchDocuments":          toolkits.ToolGroupRead,
		"onenote_get_page_content": toolkits.ToolGroupRead,
		// No recognised verb at all — the documented fallback.
		"places":          toolkits.ToolGroupRead,
		"fields_metadata": toolkits.ToolGroupRead,
		"job_stats":       toolkits.ToolGroupRead,

		// Create & update.
		"create_issue":                 toolkits.ToolGroupCreateUpdate,
		"update_issue":                 toolkits.ToolGroupCreateUpdate,
		"add_comments":                 toolkits.ToolGroupCreateUpdate,
		"set_issue_status":             toolkits.ToolGroupCreateUpdate,
		"append_data":                  toolkits.ToolGroupCreateUpdate,
		"index_data":                   toolkits.ToolGroupCreateUpdate,
		"indexDocuments":               toolkits.ToolGroupCreateUpdate,
		"onenote_replace_page_content": toolkits.ToolGroupCreateUpdate,
		"link_issues":                  toolkits.ToolGroupCreateUpdate,

		// Delete.
		"delete_file":         toolkits.ToolGroupDelete,
		"remove_index":        toolkits.ToolGroupDelete,
		"onenote_delete_page": toolkits.ToolGroupDelete,
		"unlink_work_items":   toolkits.ToolGroupDelete,

		// Execute.
		"execute_generic_rq":       toolkits.ToolGroupExecute,
		"run_pipeline":             toolkits.ToolGroupExecute,
		"generic_request":          toolkits.ToolGroupExecute,
		"stateful_pyodide_sandbox": toolkits.ToolGroupExecute,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := toolkits.ClassifyToolGroup(name); got != want {
				t.Fatalf("ClassifyToolGroup(%q)=%q, want %q", name, got, want)
			}
		})
	}
}

// The FIRST recognised verb wins. This is the rule, asserted as a rule rather
// than through examples, because it is the only thing that keeps the
// search/index pair apart.
func TestClassifyToolGroupTakesTheFirstRecognisedVerb(t *testing.T) {
	t.Parallel()

	if toolkits.ClassifyToolGroup("search_index") != toolkits.ToolGroupRead {
		t.Fatal("search_index must be a read: it searches an index")
	}
	if toolkits.ClassifyToolGroup("index_data") != toolkits.ToolGroupCreateUpdate {
		t.Fatal("index_data must be a write: it indexes data")
	}
	if toolkits.ClassifyToolGroup("delete_and_create") != toolkits.ToolGroupDelete {
		t.Fatal("the leading verb decides")
	}
	if toolkits.ClassifyToolGroup("create_and_delete") != toolkits.ToolGroupCreateUpdate {
		t.Fatal("the leading verb decides")
	}
	if toolkits.ClassifyToolGroup("") != toolkits.ToolGroupRead {
		t.Fatal("an empty name falls to the read fallback rather than panicking")
	}
}

// The served contract, end to end through the real handler and the real pinned
// SDK snapshot: every tool the picker renders must carry a group, the fixed
// order must be served with it, and the groups must be the ones a person would
// recognise.
func TestToolkitTypeCatalogueServesAToolGroupForEverySDKTool(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))

	for _, toolkitType := range []string{"github", "jira", "artifact"} {
		selectedTools := selectedToolsSchema(t, body, toolkitType)
		argsSchemas, ok := selectedTools["args_schemas"].(map[string]any)
		if !ok || len(argsSchemas) == 0 {
			t.Fatalf("%s has no args_schemas to group", toolkitType)
		}
		groups, ok := selectedTools["tool_groups"].(map[string]any)
		if !ok {
			t.Fatalf("%s selected_tools carries no tool_groups: %v", toolkitType, keysOf(selectedTools))
		}
		// EVERY rendered tool, not merely some: a tool with no group falls
		// out of the grouped view entirely.
		for tool := range argsSchemas {
			group, present := groups[tool].(string)
			if !present {
				t.Errorf("%s tool %q has no group", toolkitType, tool)
				continue
			}
			switch toolkits.ToolGroup(group) {
			case toolkits.ToolGroupRead, toolkits.ToolGroupCreateUpdate,
				toolkits.ToolGroupDelete, toolkits.ToolGroupExecute:
			default:
				t.Errorf("%s tool %q has unknown group %q", toolkitType, tool, group)
			}
		}
		if len(groups) != len(argsSchemas) {
			t.Errorf("%s: %d groups for %d tools — the map must not name a tool the picker does not render",
				toolkitType, len(groups), len(argsSchemas))
		}
		order, ok := selectedTools["tool_group_order"].([]any)
		if !ok || len(order) != 4 ||
			order[0] != "read" || order[1] != "create_update" ||
			order[2] != "delete" || order[3] != "execute" {
			t.Errorf("%s tool_group_order=%#v", toolkitType, selectedTools["tool_group_order"])
		}
	}

	// Named classifications on a real type, so a regression in the verb table
	// fails here rather than only in the UI.
	jira := selectedToolsSchema(t, body, "jira")
	jiraGroups, _ := jira["tool_groups"].(map[string]any)
	for tool, want := range map[string]string{
		"get_remote_links":   "read",
		"search_using_jql":   "read",
		"create_issue":       "create_update",
		"update_issue":       "create_update",
		"remove_index":       "delete",
		"execute_generic_rq": "execute",
	} {
		if jiraGroups[tool] != want {
			t.Errorf("jira %q group=%v, want %q", tool, jiraGroups[tool], want)
		}
	}

	// The artifact toolkit offers nothing that runs caller-supplied code, so
	// its Execute group must come out EMPTY — the case that proves the
	// classification is not labelling everything.
	artifact := selectedToolsSchema(t, body, "artifact")
	artifactGroups, _ := artifact["tool_groups"].(map[string]any)
	executeTools := []string{}
	for tool, group := range artifactGroups {
		if group == "execute" {
			executeTools = append(executeTools, tool)
		}
	}
	sort.Strings(executeTools)
	if len(executeTools) != 0 {
		t.Errorf("artifact execute tools=%v, want none", executeTools)
	}
	if artifactGroups["delete_file"] != "delete" || artifactGroups["read_file"] != "read" {
		t.Errorf("artifact groups: delete_file=%v read_file=%v", artifactGroups["delete_file"], artifactGroups["read_file"])
	}
}

// A type whose tools are discovered at RUN TIME (Remote MCP) has no tool list
// in the catalogue, so it must carry no groups at all. "No answer" and
// "everything is a read" must not look the same: the flat-list fallback in the
// picker is keyed on exactly this absence (ELITEA-2688).
func TestToolkitTypeCatalogueServesNoToolGroupsForRuntimeDiscoveredTools(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))
	selectedTools := selectedToolsSchema(t, body, "mcp")
	if _, present := selectedTools["tool_groups"]; present {
		t.Fatalf("the Remote MCP type carries tool_groups: %#v", selectedTools["tool_groups"])
	}
	if _, present := selectedTools["tool_group_order"]; present {
		t.Fatalf("the Remote MCP type carries tool_group_order")
	}
}
