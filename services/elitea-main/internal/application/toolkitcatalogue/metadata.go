package toolkitcatalogue

// The label and category the admin page groups a toolkit type under.
//
// # Why this table exists, and when it should stop existing
//
// The served catalogue's own `metadata` block — label, categories, icon_url,
// hidden — is what production sends and is the right long-term source. The
// pinned SDK snapshot in internal/runtimecomposition does not carry it yet:
// the generator keeps the settings ANNOTATIONS and the per-tool argument
// schemas, and drops the top-level metadata. Adding it is a separate change to
// the snapshot generator and to the served catalogue.
//
// So the admin page had two options. Render every type in one undifferentiated
// list with its raw key as its name — fifty rows called `zephyr_essential` and
// `ado_boards`, no grouping, no search that matches what an operator calls the
// thing — or carry a fallback table. This is the fallback table, and it is
// explicitly a FALLBACK: `Lookup` is what the admin handler calls, and the
// handler prefers an injected metadata source when the composition root has
// one. When the snapshot starts carrying metadata, this table's entries stop
// being read and it can be deleted in that change.
//
// # It is transcribed from what production serves, not invented
//
// The category assignments below reproduce the twelve headings of production's
// "+ Toolkit" chooser at https://next.elitea.ai/app/toolkits/create, surveyed
// 2026-09-06. A type this table does not know is NOT dropped and is NOT hidden:
// it renders under `other` with a label derived from its key. A new SDK type
// must appear on the admin page the day it appears in the catalogue, or the
// page becomes a second thing to remember to update — which is how a control
// surface silently stops covering the thing it controls.

import "strings"

// Category groups types on the admin page. The values are stable identifiers;
// the web bundle owns their display text.
type Category string

const (
	CategoryCodeRepositories Category = "code_repositories"
	CategoryCommunication    Category = "communication"
	CategoryDevelopment      Category = "development"
	CategoryDocumentation    Category = "documentation"
	CategoryIntegrations     Category = "integrations"
	CategoryMCP              Category = "mcp"
	CategoryOffice           Category = "office"
	CategoryProjectMgmt      Category = "project_management"
	CategoryStorage          Category = "storage"
	CategoryTestManagement   Category = "test_management"
	CategoryTesting          Category = "testing"
	CategoryInternal         Category = "internal"
	CategoryOther            Category = "other"
)

// TypeMetadata is what the page shows for one type before any policy applies.
type TypeMetadata struct {
	Label    string
	Category Category
}

// knownTypeMetadata carries the label and category for every type the pinned
// SDK snapshot and the elitea_core-native set contain.
//
// `internal` is this table's own category, not production's. Production marks
// these `metadata.hidden` and the chooser drops them; the ADMIN page shows them,
// because an operator asking "what can this platform build" is asking about the
// whole registry, and a control page that hid half of it would be the deny-list
// problem again in a new place.
var knownTypeMetadata = map[string]TypeMetadata{
	// Code repositories.
	"ado_repos":  {Label: "ADO Repos", Category: CategoryCodeRepositories},
	"bitbucket":  {Label: "Bitbucket", Category: CategoryCodeRepositories},
	"github":     {Label: "GitHub", Category: CategoryCodeRepositories},
	"gitlab":     {Label: "GitLab", Category: CategoryCodeRepositories},
	"gitlab_org": {Label: "GitLab Org", Category: CategoryCodeRepositories},
	"localgit":   {Label: "Local Git", Category: CategoryCodeRepositories},

	// Communication.
	"slack":   {Label: "Slack", Category: CategoryCommunication},
	"yagmail": {Label: "Yagmail", Category: CategoryCommunication},

	// Development.
	"sonar":         {Label: "Sonar", Category: CategoryDevelopment},
	"sql":           {Label: "SQL", Category: CategoryDevelopment},
	"database":      {Label: "Database", Category: CategoryDevelopment},
	"data_analysis": {Label: "Data Analysis", Category: CategoryDevelopment},

	// Documentation.
	"ado_wiki":   {Label: "ADO Wiki", Category: CategoryDocumentation},
	"confluence": {Label: "Confluence", Category: CategoryDocumentation},

	// Integrations.
	"openapi": {Label: "OpenAPI", Category: CategoryIntegrations},
	"custom":  {Label: "Custom", Category: CategoryIntegrations},

	// MCP.
	"mcp":        {Label: "Remote MCP", Category: CategoryMCP},
	"mcp_config": {Label: "MCP Configuration", Category: CategoryMCP},

	// Office.
	"pptx":       {Label: "PPTX", Category: CategoryOffice},
	"sharepoint": {Label: "SharePoint", Category: CategoryOffice},

	// Project management.
	"ado_boards": {Label: "ADO Boards", Category: CategoryProjectMgmt},
	"aha":        {Label: "Aha!", Category: CategoryProjectMgmt},
	"jira":       {Label: "Jira", Category: CategoryProjectMgmt},
	"rally":      {Label: "Rally", Category: CategoryProjectMgmt},

	// Storage.
	"artifact":    {Label: "Artifact", Category: CategoryStorage},
	"datasource":  {Label: "Datasource", Category: CategoryStorage},
	"vectorstore": {Label: "Vector Store", Category: CategoryStorage},
	"bigquery":    {Label: "BigQuery", Category: CategoryStorage},
	"delta_lake":  {Label: "Delta Lake", Category: CategoryStorage},

	// Test management.
	"ado_plans":         {Label: "ADO Plans", Category: CategoryTestManagement},
	"qtest":             {Label: "QTest", Category: CategoryTestManagement},
	"testrail":          {Label: "TestRail", Category: CategoryTestManagement},
	"xray_cloud":        {Label: "XRAY Cloud", Category: CategoryTestManagement},
	"zephyr":            {Label: "Zephyr (legacy)", Category: CategoryTestManagement},
	"zephyr_enterprise": {Label: "Zephyr Enterprise", Category: CategoryTestManagement},
	"zephyr_essential":  {Label: "Zephyr Essential", Category: CategoryTestManagement},
	"zephyr_scale":      {Label: "Zephyr Scale", Category: CategoryTestManagement},
	"zephyr_squad":      {Label: "Zephyr Squad", Category: CategoryTestManagement},

	// Testing.
	"carrier":       {Label: "Carrier", Category: CategoryTesting},
	"report_portal": {Label: "Report Portal", Category: CategoryTesting},
	"testio":        {Label: "TestIO", Category: CategoryTesting},

	// Internal / infrastructure. Production hides these from the chooser.
	"aws":          {Label: "AWS", Category: CategoryInternal},
	"azure":        {Label: "Azure", Category: CategoryInternal},
	"azure_search": {Label: "Azure Search", Category: CategoryInternal},
	"elastic":      {Label: "Elastic", Category: CategoryInternal},
	"gcp":          {Label: "GCP", Category: CategoryInternal},
	"keycloak":     {Label: "Keycloak", Category: CategoryInternal},
	"kubernetes":   {Label: "Kubernetes", Category: CategoryInternal},
	"sandbox":      {Label: "Sandbox", Category: CategoryInternal},
	"application":  {Label: "Application", Category: CategoryInternal},

	// Other.
	"figma":         {Label: "Figma", Category: CategoryOther},
	"google_places": {Label: "Google Places", Category: CategoryOther},
	"memory":        {Label: "Memory", Category: CategoryOther},
	"postman":       {Label: "Postman", Category: CategoryOther},
	"salesforce":    {Label: "Salesforce", Category: CategoryOther},
	"service_now":   {Label: "ServiceNow", Category: CategoryOther},
}

// Lookup answers for any type, known or not.
//
// A projected pre-built MCP type (`mcp_<key>`) is categorised as MCP by its
// prefix rather than by an entry per server: those rows are operator-authored
// and the table cannot know them. Everything else unknown lands in `other` with
// a derived label.
func Lookup(toolkitType string) TypeMetadata {
	key := strings.TrimSpace(toolkitType)
	if metadata, found := knownTypeMetadata[key]; found {
		return metadata
	}
	if strings.HasPrefix(key, "mcp_") {
		return TypeMetadata{Label: DeriveLabel(strings.TrimPrefix(key, "mcp_")), Category: CategoryMCP}
	}
	return TypeMetadata{Label: DeriveLabel(key), Category: CategoryOther}
}

// DeriveLabel turns a snake_case key into title case.
//
// It is deliberately dumb. A clever transformation that produced "Ado Repos"
// for a known type would be worse than the table above, and one that guessed
// acronyms would produce a different wrong answer per type. This runs only for
// types the table does not know, where any answer is a guess and the honest one
// is the key made readable.
func DeriveLabel(toolkitType string) string {
	key := strings.TrimSpace(toolkitType)
	if key == "" {
		return ""
	}
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' })
	for index, word := range words {
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// Categories lists every category identifier, in the order the page renders
// them. Exported so the web bundle's filter list and this table cannot drift.
func Categories() []Category {
	return []Category{
		CategoryCodeRepositories,
		CategoryCommunication,
		CategoryDevelopment,
		CategoryDocumentation,
		CategoryIntegrations,
		CategoryMCP,
		CategoryOffice,
		CategoryProjectMgmt,
		CategoryStorage,
		CategoryTestManagement,
		CategoryTesting,
		CategoryInternal,
		CategoryOther,
	}
}
