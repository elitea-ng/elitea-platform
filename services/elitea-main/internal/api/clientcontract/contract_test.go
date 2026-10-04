package clientcontract_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/EliteaAI/elitea-platform/services/elitea-main/api/openapi"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/clientcontract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
)

// update rewrites the lock of the current major from the spec. It is the only
// sanctioned way to change a lock, and it refuses to record a breaking change.
var update = flag.Bool("update", false, "rewrite api/openapi/client-contract/v<major>.lock.json from v2.yaml (additive changes only)")

const lockDir = "../../../api/openapi/client-contract"

// minLockedOperations is a floor on the lock itself. A lock that shrank to a
// handful of operations — or to none — would make every comparison pass. 22
// operations carried the tag when the v1 lock was first written.
const minLockedOperations = 22

// adrOperations is ADR-0025 decision 6's list, spelled as operation ids. Every
// one must be in the lock: a gate that compares an incomplete lock against an
// incomplete spec passes for the wrong reason.
var adrOperations = []string{
	// discovery and brand pack
	"getClientDiscovery", "getBrandingPackJSON",
	// native auth
	"authorizeNativeClient", "exchangeNativeToken", "revokeNativeToken",
	// device registry
	"listNativeDevices", "revokeNativeDevice",
	// project list
	"listProjects",
	// conversation list / detail / create / delete
	"listConversations", "getConversation", "createConversation", "deleteConversation",
	// message list
	"listConversationMessages",
	// chat send / regenerate / continue
	"sendChatMessage", "regenerateChatMessage", "continueChatExecution",
	// execution event stream
	"streamExecutionEvents",
	// attachment upload
	"uploadConversationAttachment",
	// notification list / update and its stream
	"listNotifications", "markNotificationsSeen", "markNotificationSeen", "streamNotificationEvents",
	// The policy has no operation of its own: it is delivered inside the
	// discovery document and the token response, both locked above.
}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatalf("loading the embedded v2.yaml: %v", err)
	}
	return doc
}

func currentSurface(t *testing.T) *clientcontract.Surface {
	t.Helper()
	surface, err := clientcontract.Normalize(loadSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	return surface
}

var lockName = regexp.MustCompile(`^v([0-9]+)\.lock\.json$`)

// lockMajors lists the majors that have a lock file, ascending.
func lockMajors(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		t.Fatalf("reading %s: %v", lockDir, err)
	}
	var majors []int
	for _, entry := range entries {
		if m := lockName.FindStringSubmatch(entry.Name()); m != nil {
			major, _ := strconv.Atoi(m[1])
			majors = append(majors, major)
		}
	}
	sort.Ints(majors)
	if len(majors) == 0 {
		t.Fatalf("%s holds no v<N>.lock.json: the client contract has no recorded promise, so nothing could fail", lockDir)
	}
	return majors
}

func lockPath(major string) string {
	return filepath.Join(lockDir, "v"+major+".lock.json")
}

func readLock(t *testing.T, major string) *clientcontract.Surface {
	t.Helper()
	data, err := os.ReadFile(lockPath(major))
	if err != nil {
		t.Fatalf("reading the v%s lock: %v", major, err)
	}
	lock, err := clientcontract.Unmarshal(data)
	if err != nil {
		t.Fatalf("parsing %s: %v", lockPath(major), err)
	}
	return lock
}

func specMajor(t *testing.T, surface *clientcontract.Surface) string {
	t.Helper()
	major, err := clientcontract.Major(surface.ClientContract)
	if err != nil {
		t.Fatalf("info.%s: %v", clientcontract.VersionExtension, err)
	}
	return major
}

// TestClientContractIsAdditiveOnly is the gate: the spec's client subset must
// keep every promise the lock of its major records.
func TestClientContractIsAdditiveOnly(t *testing.T) {
	current := currentSurface(t)
	major := specMajor(t, current)
	lock := readLock(t, major)
	if problems := clientcontract.Compare(lock, current); len(problems) > 0 {
		t.Fatalf("v2.yaml breaks client contract v%s in %d place(s):\n  %s\n\n"+
			"The `client` subset is additive-only within a major (ADR-0025 decision 6). Either make the change additive, "+
			"or bump info.%s to the next major, add v<next>.lock.json with -update, keep %s, and serve the old version for "+
			"two minor releases (API_CONTRACT.md, \"Client contract\").",
			major, len(problems), strings.Join(problems, "\n  "), clientcontract.VersionExtension, lockPath(major))
	}
}

// TestClientContractLockIsCurrent makes additions visible: a spec that grew
// past its lock fails until the lock is regenerated in the same change, so the
// new promise is reviewed as a diff of the lock.
func TestClientContractLockIsCurrent(t *testing.T) {
	current := currentSurface(t)
	major := specMajor(t, current)
	want, err := clientcontract.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	path := lockPath(major)
	got, readErr := os.ReadFile(path)

	if *update {
		if readErr == nil {
			lock, err := clientcontract.Unmarshal(got)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			if problems := clientcontract.Compare(lock, current); len(problems) > 0 {
				t.Fatalf("refusing to -update %s over a BREAKING change:\n  %s", path, strings.Join(problems, "\n  "))
			}
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%s)", path, clientcontract.Summary(current))
		return
	}
	if readErr != nil {
		t.Fatalf("reading %s: %v (create it with: go test ./internal/api/clientcontract -run TestClientContract -update)", path, readErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is behind v2.yaml's client subset. If TestClientContractIsAdditiveOnly passes, the change is additive: "+
			"run `go test ./internal/api/clientcontract -run TestClientContract -update` and commit the lock with the spec.", path)
	}
}

// TestClientContractVersionAgrees ties the three statements of the version
// together: the document's extension, the newest lock file, and what the
// discovery document serves to clients.
func TestClientContractVersionAgrees(t *testing.T) {
	current := currentSurface(t)
	major := specMajor(t, current)
	majors := lockMajors(t)

	newest := strconv.Itoa(majors[len(majors)-1])
	if major != newest {
		t.Errorf("info.%s is %q (major %s) but the newest lock is v%s.lock.json", clientcontract.VersionExtension, current.ClientContract, major, newest)
	}
	// A major bump KEEPS the previous lock: the previous version stays served
	// for two minor server releases, and its lock is the record of it.
	for want := 1; want <= majors[len(majors)-1]; want++ {
		if !containsInt(majors, want) {
			t.Errorf("v%d.lock.json is missing: a lock is kept when its major is superseded", want)
		}
	}
	lock := readLock(t, major)
	if lockMajor, err := clientcontract.Major(lock.ClientContract); err != nil || lockMajor != major {
		t.Errorf("%s records client_contract %q; it must be major %s (err %v)", lockPath(major), lock.ClientContract, major, err)
	}
	if discovery.ClientContract != current.ClientContract {
		t.Errorf("discovery serves client_contract %q but v2.yaml declares %q (internal/api/v2/discovery/document.go)",
			discovery.ClientContract, current.ClientContract)
	}
}

// TestClientContractLockCoversTheADR keeps the gate from passing vacuously.
func TestClientContractLockCoversTheADR(t *testing.T) {
	lock := readLock(t, specMajor(t, currentSurface(t)))
	if len(lock.Operations) < minLockedOperations {
		t.Fatalf("the lock holds %d operations (floor %d): %s", len(lock.Operations), minLockedOperations, clientcontract.Summary(lock))
	}
	locked := map[string]bool{}
	for _, op := range lock.Operations {
		locked[op.OperationID] = true
	}
	for _, id := range adrOperations {
		if !locked[id] {
			t.Errorf("ADR-0025 decision 6 lists %s, but the lock does not hold it (tag it `client`, last, and -update)", id)
		}
	}
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- the normalizer and comparer on tiny fixtures ---------------------------

// fixture is a minimal document with one client operation that exercises
// every rule: a path, query and header parameter; a request body with a
// required and an optional property, an enum and a nullable field; responses
// with a required and optional property, a nullable field, an enum, a header
// and two media types; a $ref and an allOf.
const fixture = `
openapi: "3.0.3"
info:
  title: fixture
  version: "1"
  x-elitea-client-contract: "1.0"
paths:
  /things/{id}:
    parameters:
      - name: id
        in: path
        required: true
        schema: { type: integer }
    post:
      operationId: putThing
      tags: [things, client]
      parameters:
        - name: mode
          in: query
          required: false
          schema: { type: string, enum: [fast, safe] }
        - name: X-Trace
          in: header
          required: false
          schema: { type: string }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: { type: string }
                note: { type: string, nullable: true }
                kind: { type: string, enum: [a, b] }
                meta:
                  type: object
                  additionalProperties: true
      responses:
        '200':
          description: ok
          headers:
            ETag:
              schema: { type: string }
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Thing'
            text/plain:
              schema: { type: string }
        '409':
          description: conflict
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Err' }
  /other:
    get:
      operationId: notInContract
      tags: [things]
      responses:
        '200': { description: ok }
components:
  schemas:
    Err:
      type: object
      required: [error]
      properties:
        error: { type: string }
    Base:
      type: object
      required: [id]
      properties:
        id: { type: integer, format: int64 }
    Thing:
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          required: [name, state]
          properties:
            name: { type: string }
            state: { type: string, enum: [new, done] }
            label: { type: string }
            parent: { type: string, nullable: false }
            children:
              type: array
              items: { $ref: '#/components/schemas/Thing' }
`

func normalizeYAML(t *testing.T, doc string) *clientcontract.Surface {
	t.Helper()
	loader := openapi3.NewLoader()
	parsed, err := loader.LoadFromData([]byte(doc))
	if err != nil {
		t.Fatalf("fixture does not load: %v\n%s", err, doc)
	}
	surface, err := clientcontract.Normalize(parsed)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return surface
}

// edit applies one textual change and fails the test if the anchor is not
// found exactly once — a fixture edit that silently did nothing would make a
// breaking case pass for the wrong reason.
func edit(t *testing.T, old, replacement string) string {
	t.Helper()
	if n := strings.Count(fixture, old); n != 1 {
		t.Fatalf("fixture anchor %q found %d times, want 1", old, n)
	}
	return strings.Replace(fixture, old, replacement, 1)
}

func TestNormalizeTheFixture(t *testing.T) {
	surface := normalizeYAML(t, fixture)
	if surface.ClientContract != "1.0" {
		t.Errorf("client contract = %q", surface.ClientContract)
	}
	if len(surface.Operations) != 1 {
		t.Fatalf("want exactly the one `client` operation, got %s", clientcontract.Summary(surface))
	}
	op := surface.Operations["POST /things/{id}"]
	if op == nil || op.OperationID != "putThing" {
		t.Fatalf("operation key: %+v", surface.Operations)
	}
	if p := op.Parameters["path:id"]; p == nil || !p.Required || p.Schema.Type != "integer" {
		t.Errorf("path-level parameter not carried onto the operation: %+v", p)
	}
	if op.Parameters["header:x-trace"] == nil {
		t.Errorf("header parameter not keyed by its lower-cased name: %+v", op.Parameters)
	}
	thing := op.Responses["200"].Content["application/json"]
	if thing.Type != "object" || strings.Join(thing.Required, ",") != "id,name,state" {
		t.Errorf("allOf not merged: type %q required %v", thing.Type, thing.Required)
	}
	if thing.Properties["id"] == nil || thing.Properties["id"].Format != "int64" {
		t.Errorf("allOf $ref branch's properties missing: %+v", thing.Properties)
	}
	if child := thing.Properties["children"].Items; child == nil || child.Ref != "Thing" {
		t.Errorf("the recursive reference must be cut, got %+v", child)
	}
	if op.Responses["200"].Headers["etag"] == nil {
		t.Errorf("response header missing: %+v", op.Responses["200"].Headers)
	}
}

func TestNormalizeRefusesClientNotLast(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(edit(t, "tags: [things, client]", "tags: [client, things]")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientcontract.Normalize(doc); err == nil || !strings.Contains(err.Error(), "LAST tag") {
		t.Fatalf("`client` as the first tag must be refused, got %v", err)
	}
}

// TestCompareFailsEveryBreakingKind is the proof that the gate is not
// vacuous: one fixture per rule, each of which must be reported.
func TestCompareFailsEveryBreakingKind(t *testing.T) {
	locked := normalizeYAML(t, fixture)
	cases := []struct {
		name, old, new, want string
	}{
		{"operation untagged", "tags: [things, client]", "tags: [things]", "no longer tagged `client`"},
		{"operation removed", "    post:\n      operationId: putThing", "    put:\n      operationId: putThing", "operation is gone"},
		{"parameter removed", "        - name: X-Trace\n          in: header\n          required: false\n          schema: { type: string }\n", "", "parameter header:x-trace: removed"},
		{"parameter became required", "        - name: mode\n          in: query\n          required: false", "        - name: mode\n          in: query\n          required: true", "parameter query:mode: became required"},
		{"new required parameter", "        - name: X-Trace\n", "        - name: page\n          in: query\n          required: true\n          schema: { type: integer }\n        - name: X-Trace\n", "parameter query:page: is new and required"},
		{"parameter type changed", "schema: { type: integer }\n    post:", "schema: { type: string }\n    post:", `type changed from "integer" to "string"`},
		{"parameter enum lost a member", "enum: [fast, safe]", "enum: [fast]", `enum member "safe" removed`},
		{"request property became required", "              required: [name]\n              properties:\n                name: { type: string }\n                note:", "              required: [name, note]\n              properties:\n                name: { type: string }\n                note:", ".note: became required"},
		{"new required request property", "              required: [name]\n              properties:\n", "              required: [name, extra]\n              properties:\n                extra: { type: string }\n", ".extra: is a new required property"},
		{"request property removed", "                note: { type: string, nullable: true }\n", "", ".note: property removed"},
		{"request no longer accepts null", "note: { type: string, nullable: true }", "note: { type: string }", "no longer accepts null"},
		{"request property format changed", "                name: { type: string }\n                note:", "                name: { type: string, format: uuid }\n                note:", `format changed from "" to "uuid"`},
		{"request object closed", "                  additionalProperties: true", "                  additionalProperties: false", "no longer admits keys"},
		{"request body media type removed", "      requestBody:\n        required: true\n        content:\n          application/json:", "      requestBody:\n        required: true\n        content:\n          application/xml:", "media type removed"},
		{"response status removed", "        '409':\n          description: conflict\n          content:\n            application/json:\n              schema: { $ref: '#/components/schemas/Err' }\n", "", "response 409: status removed"},
		{"response media type removed", "            text/plain:\n              schema: { type: string }\n", "", "text/plain: media type removed"},
		{"response header removed", "          headers:\n            ETag:\n              schema: { type: string }\n", "", "header etag: removed"},
		{"response property removed", "            label: { type: string }\n", "", ".label: property removed"},
		{"response property no longer required", "          required: [name, state]", "          required: [name]", ".state: is no longer guaranteed"},
		{"response may now be null", "parent: { type: string, nullable: false }", "parent: { type: string, nullable: true }", "may now be null"},
		{"response enum lost a member", "enum: [new, done]", "enum: [new]", `enum member "done" removed`},
		{"response type changed", "        id: { type: integer, format: int64 }", "        id: { type: string, format: int64 }", `type changed from "integer" to "string"`},
		{"nested $ref property removed", "      required: [error]\n      properties:\n        error: { type: string }", "      properties:\n        message: { type: string }", ".error: property removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := normalizeYAML(t, edit(t, tc.old, tc.new))
			problems := clientcontract.Compare(locked, changed)
			if len(problems) == 0 {
				t.Fatalf("a breaking change passed the gate")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("want a problem containing %q, got:\n  %s", tc.want, strings.Join(problems, "\n  "))
			}
		})
	}
}

// TestCompareAcceptsAdditiveChanges is the other half: these grow the
// contract and must pass Compare, while still differing from the lock (so
// TestClientContractLockIsCurrent asks for the lock to be regenerated).
func TestCompareAcceptsAdditiveChanges(t *testing.T) {
	locked := normalizeYAML(t, fixture)
	lockedBytes, _ := clientcontract.Marshal(locked)
	cases := []struct{ name, old, new string }{
		{"new optional parameter", "        - name: X-Trace\n", "        - name: page\n          in: query\n          schema: { type: integer }\n        - name: X-Trace\n"},
		{"new optional request property", "                kind: { type: string, enum: [a, b] }", "                kind: { type: string, enum: [a, b] }\n                extra: { type: string }"},
		{"request property no longer required", "              required: [name]\n", ""},
		{"request accepts null", "                name: { type: string }\n                note:", "                name: { type: string, nullable: true }\n                note:"},
		{"request enum gained a member", "enum: [a, b]", "enum: [a, b, c]"},
		{"new response property", "            label: { type: string }", "            label: { type: string }\n            colour: { type: string }"},
		{"response property became required", "          required: [name, state]", "          required: [name, state, label]"},
		{"response enum gained a member", "enum: [new, done]", "enum: [new, done, archived]"},
		{"new response status", "        '409':", "        '404':\n          description: missing\n        '409':"},
		{"new client operation", "    get:\n      operationId: notInContract\n      tags: [things]", "    get:\n      operationId: notInContract\n      tags: [things, client]"},
		{"description reworded", "description: conflict", "description: a conflicting write"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := normalizeYAML(t, edit(t, tc.old, tc.new))
			if problems := clientcontract.Compare(locked, changed); len(problems) > 0 {
				t.Fatalf("an additive change was refused:\n  %s", strings.Join(problems, "\n  "))
			}
			changedBytes, _ := clientcontract.Marshal(changed)
			grew := !bytes.Equal(lockedBytes, changedBytes)
			if tc.name == "description reworded" {
				if grew {
					t.Fatalf("a description is not part of the normalized surface, but the lock would change")
				}
				return
			}
			if !grew {
				t.Fatalf("the change is invisible to the lock, so -update would never record it")
			}
		})
	}
}

// TestUnmarshalRefusesAnEmptyLock: a lock that lost its operations must not
// read as "nothing promised".
func TestUnmarshalRefusesAnEmptyLock(t *testing.T) {
	for _, data := range []string{`{}`, `{"client_contract":"1.0"}`, `{"client_contract":"1.0","operations":{}}`, `{"client_contract":"1.0","operations":{"GET /x":{"operation_id":"x","responses":{}}},"extra":1}`} {
		if _, err := clientcontract.Unmarshal([]byte(data)); err == nil {
			t.Errorf("Unmarshal(%s) accepted a lock it must refuse", data)
		}
	}
}

func TestMajor(t *testing.T) {
	for version, want := range map[string]string{"1.0": "1", "2.13": "2"} {
		if got, err := clientcontract.Major(version); err != nil || got != want {
			t.Errorf("Major(%q) = %q, %v", version, got, err)
		}
	}
	for _, bad := range []string{"", "1", "v1.0", "1.x", ".1", "1."} {
		if _, err := clientcontract.Major(bad); err == nil {
			t.Errorf("Major(%q) accepted", bad)
		}
	}
}
