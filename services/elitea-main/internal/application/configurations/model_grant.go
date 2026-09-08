package configurations

// The GRANT SCOPE of a platform (catalogue) model.
//
// # What `shared` did, and what it could not say
//
// A catalogue model is a row in the public project's schema with
// `shared = true`. That flag is a single bit and it answers a single question:
// is this row a PLATFORM row. Every reader that merges the catalogue into a
// project's model list keyed on it, so publishing a model published it to
// every project on the deployment and withdrawing it withdrew it from all of
// them. There was no third state, and an operator who wanted one model for two
// projects had to copy the row into each of them by hand — which makes two
// models that drift.
//
// The scope NARROWS `shared`; it does not replace it. `shared = true` still
// means "this is a platform row", so nothing that keys on the column changes
// meaning, and the scope decides WHICH projects the row is offered to.
//
// # Where it lives, and why absence means "all"
//
// Two fields on the row's own `data` object:
//
//	"share_scope":  "all" | "none" | "projects"
//	"shared_with":  [12, 34]        // read only when share_scope is "projects"
//
// A row written before this feature carries NEITHER, and every such row was
// offered to every project. So an absent or unreadable scope is read as "all":
// the compatible answer is the one that keeps existing deployments serving the
// models they serve today. A scope that defaulted to "none" would withdraw
// every platform model on the deployment the moment this code shipped, without
// an operator having changed anything.
//
// A malformed value is read the same way, and deliberately: `share_scope: 7`
// is a row nothing on this platform writes, and refusing to serve it would
// turn a corrupt field into an outage rather than into a row that behaves as
// it did before the field existed. The write surfaces refuse a malformed value
// at the point it is authored (`global_models.go`), which is where the operator
// can still fix it.
//
// # `shared_with` holds project IDS
//
// Ids, not names: a project can be renamed, and a grant that survived a rename
// as a dangling name would silently stop applying. The ids are read leniently
// — a JSON document can carry a number as `12` or as `12.0` or, from a client
// that stringifies its ids, as `"12"` — because a grant that failed to parse
// is a grant that silently does not apply.
//
// # This type is COPIED in the gateway
//
// services/elitea-llm-gateway is a separate Go module with no import path into
// this one, and it enforces the same rule on its own model resolution. The
// copy there (internal/llmproxy/model_grant.go) states the same three scopes
// and the same absent-means-all rule; a change here needs the same change
// there, and each side has its own test.

// ModelShareScope is the stored `data.share_scope` value.
type ModelShareScope string

const (
	// ModelShareScopeAll offers the row to every project. It is the value an
	// absent scope reads as.
	ModelShareScopeAll ModelShareScope = "all"
	// ModelShareScopeNone offers the row to no project at all. The row stays in
	// the catalogue's own schema and the catalogue project still sees it, which
	// is what makes it recoverable: an operator can re-grant it.
	ModelShareScopeNone ModelShareScope = "none"
	// ModelShareScopeProjects offers the row to the ids in `shared_with`.
	ModelShareScopeProjects ModelShareScope = "projects"
)

// ModelShareScopeField and ModelSharedWithField are the two `data` keys. They
// are named once, here, because four packages read them.
const (
	ModelShareScopeField = "share_scope"
	ModelSharedWithField = "shared_with"
)

// IsSupportedModelShareScope reports whether a value is one this platform
// writes. It is the WRITE-side rule: the read side accepts anything and reads
// what it does not know as "all" (see the file header).
func IsSupportedModelShareScope(scope ModelShareScope) bool {
	switch scope {
	case ModelShareScopeAll, ModelShareScopeNone, ModelShareScopeProjects:
		return true
	default:
		return false
	}
}

// ModelGrant is one catalogue row's answer to "which projects may see this".
type ModelGrant struct {
	Scope ModelShareScope
	// Projects is meaningful only for ModelShareScopeProjects. It is kept as
	// read rather than as a set, so a caller can report it back to an operator
	// in the order it was authored.
	Projects []int32
}

// ReadModelGrant reads the grant off a row's decoded `data` object.
//
// A nil map, an absent key, an unknown scope and a malformed value all answer
// the same grant: every project. See the file header for why the lenient read
// is the compatible one.
func ReadModelGrant(data map[string]any) ModelGrant {
	if data == nil {
		return ModelGrant{Scope: ModelShareScopeAll}
	}
	raw, _ := data[ModelShareScopeField].(string)
	scope := ModelShareScope(raw)
	if !IsSupportedModelShareScope(scope) {
		return ModelGrant{Scope: ModelShareScopeAll}
	}
	if scope != ModelShareScopeProjects {
		return ModelGrant{Scope: scope}
	}
	return ModelGrant{Scope: scope, Projects: readModelGrantProjects(data[ModelSharedWithField])}
}

// readModelGrantProjects reads `shared_with` into project ids.
//
// Every JSON spelling of an integer is accepted — `json.Unmarshal` into `any`
// gives a float64, a client that stringifies ids gives a string — because a
// grant that failed to parse is a grant that silently does not apply, and the
// project it named would lose a model with no message anywhere.
func readModelGrantProjects(raw any) []int32 {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	projects := make([]int32, 0, len(values))
	for _, value := range values {
		if id, ok := modelGrantProjectID(value); ok {
			projects = append(projects, id)
		}
	}
	return projects
}

func modelGrantProjectID(value any) (int32, bool) {
	switch typed := value.(type) {
	case float64:
		return int32Of(typed)
	case int:
		return int32Of(float64(typed))
	case int32:
		return typed, typed > 0
	case int64:
		return int32Of(float64(typed))
	case string:
		return parseModelGrantProjectID(typed)
	case interface{ Int64() (int64, error) }:
		// json.Number, when a decoder was configured to produce one.
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int32Of(float64(number))
	default:
		return 0, false
	}
}

func parseModelGrantProjectID(value string) (int32, bool) {
	if value == "" {
		return 0, false
	}
	var id int64
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		id = id*10 + int64(character-'0')
		if id > int64(^uint32(0)>>1) {
			return 0, false
		}
	}
	return int32Of(float64(id))
}

// int32Of admits a whole, positive, in-range project id and nothing else. A
// zero, a negative and a fraction are all values no project id has.
func int32Of(value float64) (int32, bool) {
	if value <= 0 || value != float64(int64(value)) || value > float64(^uint32(0)>>1) {
		return 0, false
	}
	return int32(value), true
}

// Allows reports whether the catalogue row this grant belongs to may be
// offered to projectID.
//
// The CATALOGUE's own project is not asked about here. A row lives in that
// project's schema, so the project reads it as one of its own rows and never
// through the shared merge — which is what keeps a `none` row recoverable:
// the operator can still see it on the screen that granted it.
func (grant ModelGrant) Allows(projectID int32) bool {
	switch grant.Scope {
	case ModelShareScopeNone:
		return false
	case ModelShareScopeProjects:
		for _, granted := range grant.Projects {
			if granted == projectID {
				return true
			}
		}
		return false
	default:
		return true
	}
}
