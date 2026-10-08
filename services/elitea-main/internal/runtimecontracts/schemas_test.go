package runtimecontracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	schemaDir   = "../../../../libs/jsonschema/runtime/v1"
	resourceURL = "https://schemas.elitea.ai/runtime/v1/"
)

// contractFamilies are the schema families held to the strict rules below.
// Older families keep their own Go tests.
var contractFamilies = []string{"fanout-", "http-action-"}

var fixtureName = regexp.MustCompile(`^(.+)-v([0-9]+)(\.[a-z0-9-]+)?(\.invalid\.[a-z0-9-]+)?\.json$`)

type contractSchema struct {
	id       string
	compiled *jsonschema.Schema
	valid    int
	invalid  int
}

func inFamily(stem string) bool {
	for _, prefix := range contractFamilies {
		if strings.HasPrefix(stem, prefix) {
			return true
		}
	}
	return false
}

func readJSON(t *testing.T, path string) any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	value, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(path, err)
	}
	return value
}

func loadContractSchemas(t *testing.T) map[string]*contractSchema {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(schemaDir, "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	schemas := map[string]*contractSchema{}
	for _, path := range paths {
		stem := strings.TrimSuffix(filepath.Base(path), ".schema.json")
		if !inFamily(stem) {
			continue
		}
		document := readJSON(t, path)
		root, ok := document.(map[string]any)
		if !ok {
			t.Fatal(path, "schema root is not an object")
		}
		if root["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Fatal(path, "schema must declare draft 2020-12")
		}
		id, _ := root["$id"].(string)
		if !regexp.MustCompile(`^elitea\.[a-z]+\.` + regexp.QuoteMeta(stem) + `\.v[0-9]+$`).MatchString(id) {
			t.Fatal(path, "$id must be elitea.<area>.<stem>.v<N>, got", id)
		}
		lintStrict(t, stem, "#", root)
		// Register under the resolved $id so cross-schema $ref by $id resolves
		// regardless of compile order.
		if err := compiler.AddResource(resourceURL+id, document); err != nil {
			t.Fatal(path, err)
		}
		schemas[stem] = &contractSchema{id: id}
	}
	if len(schemas) == 0 {
		t.Fatal("no contract schemas found")
	}
	for stem, schema := range schemas {
		compiled, err := compiler.Compile(resourceURL + schema.id)
		if err != nil {
			t.Fatal(stem, err)
		}
		schema.compiled = compiled
	}
	return schemas
}

func typeIncludes(node map[string]any, name string) bool {
	switch kind := node["type"].(type) {
	case string:
		return kind == name
	case []any:
		for _, item := range kind {
			if item == name {
				return true
			}
		}
	}
	return false
}

var applicatorBranch = regexp.MustCompile(`/(oneOf|anyOf|allOf|not|if|then|else)(/|$)`)

// lintStrict enforces closed objects and explicit bounds on every typed
// subschema. Data-valued keywords (const, enum) are not walked. A bare
// {"type": ...} inside an applicator branch only narrows a property that is
// bounded where it is defined, so it is not a definition.
func lintStrict(t *testing.T, stem, at string, value any) {
	t.Helper()
	switch node := value.(type) {
	case []any:
		for index, item := range node {
			lintStrict(t, stem, at+"/"+jsonIndex(index), item)
		}
	case map[string]any:
		if _, typed := node["type"]; typed && len(node) == 1 && applicatorBranch.MatchString(at) {
			return
		}
		_, constant := node["const"]
		_, enumerated := node["enum"]
		if typeIncludes(node, "object") {
			closed := node["additionalProperties"] == false
			_, named := node["propertyNames"]
			_, capped := node["maxProperties"]
			_, mapLike := node["additionalProperties"].(map[string]any)
			if !closed && !(mapLike && named && capped) {
				t.Errorf("%s %s: object must set additionalProperties:false, or a schema with propertyNames and maxProperties", stem, at)
			}
		}
		if typeIncludes(node, "string") && !constant && !enumerated {
			if _, ok := node["maxLength"]; !ok {
				t.Errorf("%s %s: string needs maxLength", stem, at)
			}
		}
		if typeIncludes(node, "array") {
			if _, ok := node["maxItems"]; !ok {
				t.Errorf("%s %s: array needs maxItems", stem, at)
			}
		}
		if typeIncludes(node, "integer") || typeIncludes(node, "number") {
			_, low := node["minimum"]
			_, high := node["maximum"]
			if !low || !high {
				t.Errorf("%s %s: number needs minimum and maximum", stem, at)
			}
		}
		for key, child := range node {
			if key == "const" || key == "enum" || key == "default" || key == "examples" {
				continue
			}
			lintStrict(t, stem, at+"/"+key, child)
		}
	}
}

func jsonIndex(index int) string {
	raw, _ := json.Marshal(index)
	return string(raw)
}

// canonicalJSON matches the node-recovery receipt canonicalization: sorted
// keys, compact, exact numbers, no HTML escaping, no trailing newline.
func canonicalJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if decoder.More() {
		t.Fatal("trailing data")
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func TestRuntimeContractSchemasAndFixtures(t *testing.T) {
	schemas := loadContractSchemas(t)
	paths, err := filepath.Glob(filepath.Join(schemaDir, "fixtures", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		match := fixtureName.FindStringSubmatch(name)
		if match == nil || !inFamily(match[1]) {
			continue
		}
		schema, ok := schemas[match[1]]
		if !ok {
			t.Errorf("%s: no schema %s.schema.json", name, match[1])
			continue
		}
		if !strings.HasSuffix(schema.id, ".v"+match[2]) {
			t.Errorf("%s: fixture version v%s does not match %s", name, match[2], schema.id)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if canonical := canonicalJSON(t, raw); !bytes.Equal(canonical, raw) {
			t.Errorf("%s: fixture is not canonical JSON (sorted keys, compact, no trailing newline)", name)
		}
		err = schema.compiled.Validate(readJSON(t, path))
		if match[4] == "" {
			schema.valid++
			if err != nil {
				t.Errorf("%s: valid fixture rejected: %v", name, err)
			}
		} else {
			schema.invalid++
			if err == nil {
				t.Errorf("%s: invalid fixture accepted", name)
			}
		}
	}
	for stem, schema := range schemas {
		if schema.valid < 1 || schema.invalid < 2 {
			t.Errorf("%s: needs at least 1 valid and 2 invalid fixtures, has %d/%d", stem, schema.valid, schema.invalid)
		}
	}
}

func lengthPrefixed(value string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(value)))
	return append(out, value...)
}

func TestFanoutInterruptKeyAndMemberCallIDVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(schemaDir, "fixtures", "fanout-interrupt-key-vectors-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		InterruptKeys []struct {
			Name   string `json:"name"`
			Inputs struct {
				RootThread              string `json:"root_thread"`
				FanoutNodeID            string `json:"fanout_node_id"`
				Step                    uint64 `json:"step"`
				ConfigDigest            string `json:"config_digest"`
				Ordinal                 uint64 `json:"ordinal"`
				ChildThread             string `json:"child_thread"`
				ChildPausedCheckpointID string `json:"child_paused_checkpoint_id"`
				InterruptID             string `json:"interrupt_id"`
				ToolCallID              string `json:"tool_call_id"`
			} `json:"inputs"`
			PreimageHex  string `json:"preimage_hex"`
			InterruptKey string `json:"interrupt_key"`
		} `json:"interrupt_keys"`
		MemberCallIDs []struct {
			ChildThread string `json:"child_thread"`
			CallID      string `json:"call_id"`
		} `json:"member_call_ids"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.InterruptKeys) == 0 || len(fixture.MemberCallIDs) == 0 {
		t.Fatal("empty vectors")
	}
	for _, vector := range fixture.InterruptKeys {
		in := vector.Inputs
		preimage := []byte("elitea.graph.fanout-interrupt.v1\x00")
		preimage = append(preimage, lengthPrefixed(in.RootThread)...)
		preimage = append(preimage, lengthPrefixed(in.FanoutNodeID)...)
		preimage = binary.BigEndian.AppendUint64(preimage, in.Step)
		preimage = append(preimage, lengthPrefixed(in.ConfigDigest)...)
		preimage = binary.BigEndian.AppendUint64(preimage, in.Ordinal)
		preimage = append(preimage, lengthPrefixed(in.ChildThread)...)
		preimage = append(preimage, lengthPrefixed(in.ChildPausedCheckpointID)...)
		preimage = append(preimage, lengthPrefixed(in.InterruptID)...)
		preimage = append(preimage, lengthPrefixed(in.ToolCallID)...)
		if hex.EncodeToString(preimage) != vector.PreimageHex {
			t.Errorf("%s: preimage differs", vector.Name)
		}
		sum := sha256.Sum256(preimage)
		if hex.EncodeToString(sum[:]) != vector.InterruptKey {
			t.Errorf("%s: interrupt_key differs", vector.Name)
		}
	}
	for _, vector := range fixture.MemberCallIDs {
		sum := sha256.Sum256(append([]byte("elitea.graph.fanout-member-call.v1\x00"), lengthPrefixed(vector.ChildThread)...))
		if "fo1_"+hex.EncodeToString(sum[:]) != vector.CallID {
			t.Errorf("member call_id differs for %s", vector.ChildThread)
		}
	}
}

func TestFanoutDecisionDigestsInFetchFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(schemaDir, "fixtures", "fanout-interrupt-fetch-v1*.json"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range paths {
		if strings.Contains(filepath.Base(path), ".invalid.") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixture struct {
			Decisions []map[string]json.RawMessage `json:"decisions"`
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		for _, decision := range fixture.Decisions {
			bound := map[string]json.RawMessage{}
			for _, key := range []string{"action", "credential_ref", "interrupt_key", "request_id", "revision", "value"} {
				bound[key] = decision[key]
			}
			encoded, err := json.Marshal(bound)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(canonicalJSON(t, encoded))
			var want string
			if err := json.Unmarshal(decision["decision_sha256"], &want); err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(sum[:]) != want {
				t.Errorf("%s: decision_sha256 differs", filepath.Base(path))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no decision digests checked")
	}
}
