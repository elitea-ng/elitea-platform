package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

func TestInternalToolPropertyNamesFitModelSchemaContract(t *testing.T) {
	valid := regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	var check func(any, string)
	check = func(value any, path string) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "properties" {
					properties, ok := child.(map[string]any)
					if !ok {
						t.Fatalf("invalid properties at %s", path)
					}
					for name := range properties {
						if !valid.MatchString(name) {
							t.Errorf("invalid model property %s.%s", path, name)
						}
					}
				}
				check(child, path+"."+key)
			}
		case []any:
			for _, child := range value {
				check(child, path)
			}
		}
	}
	count := 0
	for _, builder := range mcpregistry.InternalBuilders() {
		tools, err := (postgresToolSource{}).tools(context.Background(), `"p_1"`, scope{kind: scopeCategory, category: builder.Category})
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools {
			encoded, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema any
			if err := json.Unmarshal(encoded, &schema); err != nil {
				t.Fatal(err)
			}
			check(schema, tool.Name)
			count++
		}
	}
	if count == 0 {
		t.Fatal("no internal tools inspected")
	}
}

func TestInternalModelSchemasUseEndpointProjectWithoutMutatingCatalog(t *testing.T) {
	for _, builder := range mcpregistry.InternalBuilders() {
		tools, err := (postgresToolSource{}).tools(context.Background(), `"p_1"`, scope{kind: scopeCategory, category: builder.Category})
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools {
			before, _ := json.Marshal(tool.InputSchema)
			projected := withEndpointProjectSchema(tool)
			properties, _ := projected.InputSchema["properties"].(map[string]any)
			if _, found := properties["project_id"]; found {
				t.Errorf("%s asks the model for its endpoint project", tool.Name)
			}
			required, _ := projected.InputSchema["required"].([]string)
			for _, name := range required {
				if name == "project_id" {
					t.Errorf("%s requires its endpoint project", tool.Name)
				}
			}
			after, _ := json.Marshal(tool.InputSchema)
			if string(before) != string(after) {
				t.Errorf("%s catalog was mutated", tool.Name)
			}
		}
	}
}
