package material

// One toolkit's invoke rewrite, as data.
//
// A provider's toolkits do not all reference the same thing. DeepWiki's
// three are the whole range, and they arrived one at a time: `Wikis` names
// a code toolkit in the body; `wikis_query` names a Wikis TOOLKIT whose own
// stored code toolkit is what must be expanded; `wiki_query` names nothing
// at all, because what it reads is the project's artifact bucket. Three
// rewrites hand-written per provider is three places for the callback block
// to be forgotten — and forgetting it is not visible: the body forwards, the
// provider gets no bearer, and the failure surfaces as a wiki that will not
// load.
//
// So the SHAPE is here and the NAMES stay with the facade. What a provider
// supplies is which parameter carries the reference, which of its own
// toolkit types may own one, and how to turn one id into material. What it
// cannot supply, or skip, is the order: narrow the project, resolve, expand,
// and mint LAST — a token minted before the step that refuses is a
// credential issued for work that never happened.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// Expansion turns one reference id into the material the provider receives.
//
// `owner` is the settings of the toolkit ROW the reference came out of, and
// nil when the client named the reference directly; `parameters` is the
// body's own toolkit-level configuration, which is where a client-chosen
// repository and branch travel.
type Expansion func(
	ctx context.Context, project, reference int32,
	owner map[string]any, parameters map[string]json.RawMessage,
) (any, error)

// Owner describes a reference a toolkit ROW stores rather than one the body
// names. It is what stops a query toolkit from being a way to expand any
// configuration in the project: the only id reachable through toolkit 7 is
// the one toolkit 7 itself saved.
type Owner struct {
	Toolkits ToolkitReader
	// Types are the toolkit types that may own a reference.
	Types []string
	// Keys are the settings keys the inner reference may be under, in
	// precedence order.
	Keys []string
}

// ReferenceRewriter rewrites one invoke body: expand at most one reference,
// then mint the callback grant and write the llm_settings block.
type ReferenceRewriter struct {
	// Provider names the app in the grant's description.
	Provider string
	// Field is the parameter carrying the reference. Empty means the
	// toolkit names none, and the rewrite is the callback block alone.
	Field string
	// Aliases are older spellings of Field that user data still carries.
	// They are DELETED from the forwarded body once the modern one is
	// expanded: a bare id left behind is a reference the caller still
	// controls, next to material the facade chose.
	Aliases []string
	// Output is where the expanded value lands.
	Output string
	// Owner, when set, reads the reference out of a toolkit row instead of
	// out of the body.
	Owner *Owner
	// Expand is the provider's own expansion.
	Expand Expansion
	// FolderSource lets the body name an ARTIFACT FOLDER instead of a
	// reference: a bucket in the invoking project, and an optional prefix.
	// Off by default, because most toolkits read a credentialed remote and a
	// folder is not one.
	FolderSource bool
	// Refused wraps a refusal the caller can fix; Unavailable one they
	// cannot.
	Refused     error
	Unavailable error

	Minter       Minter
	CallbackBase string
	Lifetime     time.Duration
}

// Rewrite is the Rewriter this configuration describes.
func (rw ReferenceRewriter) Rewrite(
	ctx context.Context, body io.Reader, projectID, userID int64,
) ([]byte, Grant, error) {
	envelope, err := Read(body)
	if err != nil {
		return nil, Grant{}, err
	}
	// The project id, narrowed at the point of narrowing. The route already
	// bounds it; this covers a caller that does not come through the route
	// and is local enough for CodeQL's dataflow to see
	// (go/incorrect-integer-conversion).
	project, ok := NarrowRowID(projectID)
	if !ok {
		return nil, Grant{}, fmt.Errorf("%w: project %d is out of range", rw.Refused, projectID)
	}
	if rw.Field != "" {
		named, err := rw.folderInto(envelope)
		if err != nil {
			return nil, Grant{}, err
		}
		if !named {
			if err := rw.expandInto(ctx, envelope, project); err != nil {
				return nil, Grant{}, err
			}
		}
	}
	return Settle(ctx, envelope, rw.Minter, rw.Provider, rw.CallbackBase, rw.Lifetime, projectID, userID)
}

// folderInto handles a body that names an artifact folder rather than a
// reference, and reports whether it did.
//
// THE SOURCE IS ONE OR THE OTHER. A body naming both is refused rather than
// resolved by precedence: the two name different material, and picking one
// silently is how a caller ends up with a wiki built from something they did
// not choose. The reference field is deleted along the folder path so no
// stale id travels beside the source the facade settled on.
//
// The repository is DERIVED from the validated source, never read from the
// body: a client-supplied `artifact://…` beside a folder block would be a
// second, unvalidated way to name a bucket.
func (rw ReferenceRewriter) folderInto(envelope *Envelope) (bool, error) {
	if !rw.FolderSource {
		return false, nil
	}
	parameters := envelope.Parameters()
	source, named, err := ArtifactSourceIn(parameters, rw.Refused)
	if err != nil || !named {
		return false, err
	}
	if encoded, ok := parameters[rw.Field]; ok && !IsNull(encoded) {
		return false, fmt.Errorf(
			"%w: the request names both a %s and an %s; a wiki has one source",
			rw.Refused, rw.Field, ArtifactSourceField)
	}
	if err := envelope.Set(ArtifactSourceField, source.Block()); err != nil {
		return false, err
	}
	if err := envelope.Set("repository", source.Repository()); err != nil {
		return false, err
	}
	delete(parameters, rw.Field)
	for _, alias := range rw.Aliases {
		delete(parameters, alias)
	}
	return true, nil
}

func (rw ReferenceRewriter) expandInto(ctx context.Context, envelope *Envelope, project int32) error {
	parameters := envelope.Parameters()
	field, encoded := rw.Field, parameters[rw.Field]
	for _, alias := range rw.Aliases {
		if len(encoded) > 0 && !IsNull(encoded) {
			break
		}
		field, encoded = alias, parameters[alias]
	}
	reference, err := RowID(encoded, "configuration.parameters."+field)
	if err != nil {
		return err
	}
	var owner map[string]any
	if rw.Owner != nil {
		if reference, owner, err = rw.Owner.reference(ctx, project, reference, rw.Refused, rw.Unavailable); err != nil {
			return err
		}
	}
	expanded, err := rw.Expand(ctx, project, reference, owner, parameters)
	if err != nil {
		return err
	}
	if err := envelope.Set(rw.Output, expanded); err != nil {
		return err
	}
	for _, alias := range rw.Aliases {
		delete(parameters, alias)
	}
	return nil
}

// reference reads the owning toolkit row and the id it stores.
func (o Owner) reference(
	ctx context.Context, project, toolkitID int32, refused, unavailable error,
) (int32, map[string]any, error) {
	if o.Toolkits == nil {
		return 0, nil, fmt.Errorf(
			"%w: this deployment cannot expand a toolkit reference", unavailable)
	}
	row, err := o.Toolkits.Get(ctx, project, toolkitID)
	if err != nil {
		if errors.Is(err, repos.ErrCurrentToolkitNotFound) ||
			errors.Is(err, repos.ErrInvalidCurrentToolkitRequest) {
			return 0, nil, fmt.Errorf("%w: toolkit %d in project %d", refused, toolkitID, project)
		}
		return 0, nil, fmt.Errorf("%w: %s", unavailable, err)
	}
	if !allowed(o.Types, row.Type) {
		return 0, nil, fmt.Errorf("%w: toolkit %d is a %q toolkit, which cannot own this reference",
			refused, toolkitID, row.Type)
	}
	settings := ObjectOf(row.Settings)
	for _, key := range o.Keys {
		if id, ok := RowIDOf(settings[key]); ok {
			return id, settings, nil
		}
	}
	return 0, nil, fmt.Errorf("%w: toolkit %d stores no %s, so there is nothing to expand",
		refused, toolkitID, strings.Join(o.Keys, " or "))
}

// Settle mints the callback grant, writes the llm_settings block over
// whatever the client sent, and encodes the body to forward.
//
// The lift is not optional and not a tidy-up (#727): a client's own
// llm_settings in the TOOL parameters would otherwise reach the provider
// beside the facade's block, and that block is what a provider calls back
// with.
func Settle(
	ctx context.Context, envelope *Envelope, minter Minter,
	provider, callbackBase string, lifetime time.Duration, projectID, userID int64,
) ([]byte, Grant, error) {
	grant, err := minter.Mint(ctx, userID, projectID,
		fmt.Sprintf("%s callback (project %d)", provider, projectID), lifetime)
	if err != nil {
		return nil, Grant{}, err
	}
	block := CallbackSettings(callbackBase, grant, projectID,
		String(envelope.Parameters(), "llm_model"))
	if err := envelope.LiftToolLLMSettings(block); err != nil {
		return nil, grant, err
	}
	if err := envelope.Set("llm_settings", block); err != nil {
		return nil, grant, err
	}
	rewritten, err := envelope.Encode()
	if err != nil {
		return nil, grant, err
	}
	return rewritten, grant, nil
}

// FirstText reads the first of several settings keys carrying a non-empty
// string.
func FirstText(settings map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := Text(settings[key]); value != "" {
			return value
		}
	}
	return ""
}
