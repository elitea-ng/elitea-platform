// Package indexcatalog is the frozen index catalog of a toolkit (ADR-0030 I1,
// ADR-0031): the entries elitea-main writes into a frozen toolkit object so
// the Rust worker's index tools (list_indexes, search_index, the step-back
// pair, remove_index) know which index names exist, which vector namespace
// each one is, and which embedding space it was built in.
//
// elitea-main is the only reader of index_registry, and the worker holds no
// database connection, so the catalog travels with the toolkit: next to
// `settings`, under the key Key, in the agent execution request, the
// toolkit.call_tool.v1 settings entry (not the toolkit.execute_read request:
// that path serves no index tools and freezes no catalog).
// It is written only under ELITEA_INDEXING_RUNTIME=rust; on the Python path
// the key is absent, and that absence is what tells the Rust worker the SDK
// serves the index tools there. A present key with no entries ([]) is a
// toolkit with no indexes yet: list_indexes answers "No indexed collections".
//
// This is a leaf package (no application imports) so that the freeze paths
// and the registry can all name the same key and shape.
package indexcatalog

// Key is the frozen toolkit key that carries the catalog, beside `settings`.
const Key = "indexes"

// Families are the toolkit types whose Rust families carry the index tools in
// stage I1 (ADR-0030: artifact, github, gitlab, bitbucket, ado_repos). A
// catalog is frozen only for these; the other indexing families wait for
// I2/I3 and keep the SDK worker's index tools until then.
var Families = []string{"ado_repos", "artifact", "bitbucket", "github", "gitlab"}

// Family reports whether toolkitType is an I1 indexing family.
func Family(toolkitType string) bool {
	for _, family := range Families {
		if family == toolkitType {
			return true
		}
	}
	return false
}

// Entry is one index as the worker's catalog reads it: the user's name, the
// index_registry UUID (the vector namespace), and the embedding model and
// dimension the index was stamped with. The worker embeds a query with that
// model by this name, so it is the gateway model name, not a slug.
func Entry(name, indexID, embeddingModel string, embeddingDimension int32) map[string]any {
	return map[string]any{
		"name":                name,
		"index_id":            indexID,
		"embedding_model":     embeddingModel,
		"embedding_dimension": embeddingDimension,
	}
}

// ToolNames are the five index tools the Rust worker serves inside an I1
// family under the Rust indexing runtime. index_data is not among them: an
// index run is admitted through elitea-main (index.ingest.v1), never run
// inline by a tool call.
var ToolNames = []string{
	"list_indexes",
	"remove_index",
	"search_index",
	"stepback_search_index",
	"stepback_summary_index",
}
