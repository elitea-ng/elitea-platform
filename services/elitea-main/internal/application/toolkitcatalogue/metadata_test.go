package toolkitcatalogue

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A type the table does not know must still get a row. The admin page is a
// control surface for the whole registry, so a new SDK type has to appear the
// day it appears in the catalogue — not the day someone remembers this file.
func TestUnknownTypeFallsBackRatherThanDisappearing(t *testing.T) {
	t.Parallel()

	unknown := Lookup("brand_new_service")
	require.Equal(t, "Brand New Service", unknown.Label)
	require.Equal(t, CategoryOther, unknown.Category)

	// A projected pre-built MCP server is categorised by its prefix: those rows
	// are operator-authored and this table cannot know them.
	projected := Lookup("mcp_epam_radar")
	require.Equal(t, CategoryMCP, projected.Category)
	require.Equal(t, "Epam Radar", projected.Label)
}

func TestKnownTypesCarryProductionCategories(t *testing.T) {
	t.Parallel()

	for toolkitType, want := range map[string]TypeMetadata{
		"github":     {Label: "GitHub", Category: CategoryCodeRepositories},
		"confluence": {Label: "Confluence", Category: CategoryDocumentation},
		"qtest":      {Label: "QTest", Category: CategoryTestManagement},
		"kubernetes": {Label: "Kubernetes", Category: CategoryInternal},
		"artifact":   {Label: "Artifact", Category: CategoryStorage},
	} {
		require.Equal(t, want, Lookup(toolkitType), "metadata for %q", toolkitType)
	}
}

// Every category a table entry names must be in Categories(), or the web
// bundle's filter list would silently miss a group and its rows would vanish
// from a filtered view.
func TestEveryTableCategoryIsListed(t *testing.T) {
	t.Parallel()

	listed := map[Category]bool{}
	for _, category := range Categories() {
		require.False(t, listed[category], "duplicate category %q", category)
		listed[category] = true
	}
	require.NotEmpty(t, knownTypeMetadata)
	for toolkitType, metadata := range knownTypeMetadata {
		require.True(t, listed[metadata.Category],
			"type %q names category %q, which Categories() does not list", toolkitType, metadata.Category)
		require.NotEmpty(t, metadata.Label, "type %q has no label", toolkitType)
	}
}

func TestDeriveLabel(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", DeriveLabel("  "))
	require.Equal(t, "Sql", DeriveLabel("sql"))
	require.Equal(t, "Report Portal", DeriveLabel("report-portal"))
}
