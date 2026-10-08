package executioninterrupt

import (
	"bytes"
	"embed"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaFiles are byte-identical copies of the contract schemas Main validates
// against; TestEmbeddedSchemasMatchContract fails when one drifts from
// libs/jsonschema/runtime/v1.
//
//go:embed schemas/*.schema.json
var schemaFiles embed.FS

var schemaStems = []string{
	"fanout-hierarchy",
	"fanout-interrupt-ack-request",
	"fanout-interrupt-card",
	"fanout-interrupt-decision-request",
	"fanout-member",
}

const schemaBaseURL = "https://schemas.elitea.ai/runtime/v1/"

type compiledSchemas struct {
	card     *jsonschema.Schema
	decision *jsonschema.Schema
	ack      *jsonschema.Schema
}

var loadSchemas = sync.OnceValues(func() (compiledSchemas, error) {
	compiler := jsonschema.NewCompiler()
	for _, stem := range schemaStems {
		raw, err := schemaFiles.ReadFile("schemas/" + stem + ".schema.json")
		if err != nil {
			return compiledSchemas{}, err
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return compiledSchemas{}, err
		}
		if err := compiler.AddResource(schemaBaseURL+"elitea.pipeline."+stem+".v1", document); err != nil {
			return compiledSchemas{}, err
		}
	}
	compile := func(stem string) (*jsonschema.Schema, error) {
		return compiler.Compile(schemaBaseURL + "elitea.pipeline." + stem + ".v1")
	}
	var out compiledSchemas
	var err error
	if out.card, err = compile("fanout-interrupt-card"); err != nil {
		return compiledSchemas{}, err
	}
	if out.decision, err = compile("fanout-interrupt-decision-request"); err != nil {
		return compiledSchemas{}, err
	}
	if out.ack, err = compile("fanout-interrupt-ack-request"); err != nil {
		return compiledSchemas{}, err
	}
	return out, nil
})
