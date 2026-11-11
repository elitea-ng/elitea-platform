package storage

import (
	"context"
	"sync"

	"gopkg.in/yaml.v3"
)

// SavedCodeDocument is one frozen pipeline source parsed once (R8): the
// owner, declaration and input-policy checks of a Code visit or intent read
// its node list instead of re-parsing the YAML. It is read-only after parse.
type SavedCodeDocument struct {
	instructions string
	sha          string
	root         *yaml.Node
	nodes        *yaml.Node
	err          error
}

// ParseSavedCodeDocument parses instructions once, with savedCodeRoot's
// bounds; a parse failure is kept and returned by every check.
func ParseSavedCodeDocument(instructions string) *SavedCodeDocument {
	d := &SavedCodeDocument{instructions: instructions, sha: CodeDebugSHA256([]byte(instructions))}
	d.root, d.err = savedCodeRoot(instructions)
	if d.err == nil {
		d.nodes, d.err = savedCodeNodesOf(d.root)
	}
	return d
}

// Root is the document's resolved root mapping.
func (d *SavedCodeDocument) Root() (*yaml.Node, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.root, nil
}

// Nodes is the document's bounded `nodes` sequence.
func (d *SavedCodeDocument) Nodes() (*yaml.Node, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.nodes, nil
}

type savedCodeDocumentsKey struct{}

// savedCodeDocuments memoizes the parses of one registration (its
// transaction and any bounded retry of it).
type savedCodeDocuments struct {
	mu   sync.Mutex
	docs map[string]*SavedCodeDocument
}

// maxSavedCodeDocuments bounds one registration's memo: it reads one root
// source, and a saved child at most one more.
const maxSavedCodeDocuments = 4

// WithSavedCodeDocuments gives ctx a parse memo for one registration.
func WithSavedCodeDocuments(ctx context.Context) context.Context {
	if _, ok := ctx.Value(savedCodeDocumentsKey{}).(*savedCodeDocuments); ok {
		return ctx
	}
	return context.WithValue(ctx, savedCodeDocumentsKey{}, &savedCodeDocuments{docs: map[string]*SavedCodeDocument{}})
}

// SavedCodeDocumentFor is the parse of instructions, from ctx's memo when it
// has one.
func SavedCodeDocumentFor(ctx context.Context, instructions string) *SavedCodeDocument {
	memo, ok := ctx.Value(savedCodeDocumentsKey{}).(*savedCodeDocuments)
	if !ok {
		return ParseSavedCodeDocument(instructions)
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if doc, ok := memo.docs[instructions]; ok {
		return doc
	}
	doc := ParseSavedCodeDocument(instructions)
	if len(memo.docs) < maxSavedCodeDocuments {
		memo.docs[instructions] = doc
	}
	return doc
}
