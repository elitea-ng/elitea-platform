package eliteacore_test

// A test-only reader of the blueprint format (issue 3137, FORMAT section 1):
// `elitea.agent/v1` markdown and `elitea.team/v1` team.yaml, mapped into the
// import document the web client builds. TestBlueprintsImportRoundTrip
// (blueprints_roundtrip_postgres_integration_test.go) imports every
// blueprint through it.
//
// Two sources, one shape (blueprintAsset):
//
//   - with ELITEA_BLUEPRINTS_DIR set, the builder's OUTPUT
//     (scripts/blueprints/build.mjs --out): index.json, agents/<slug>.agent.md
//     or .agent.zip, teams/<slug>.team.zip, each read as it is, a zip with
//     archive/zip;
//   - without it, the SOURCE tree under the repository's blueprints/: a team
//     folder's team.yaml may name members outside it (../../agents/x.agent.md).

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// blueprintAsset is one importable thing: a single agent or a team, with a
// reader of the files its paths name.
type blueprintAsset struct {
	label string
	team  bool
	// entry is the agent file (an agent) or team.yaml (a team), as a path
	// read resolves.
	entry string
	read  func(name string) ([]byte, error)
}

// blueprintAgentFile is one parsed `.agent.md`.
type blueprintAgentFile struct {
	front map[string]any
	body  string
	// dir is the folder of the file, for paths relative to it.
	dir string
}

// parseBlueprintAgent splits front matter and body and decodes the front
// matter through JSON, so numbers compare as the server sees them.
func parseBlueprintAgent(t *testing.T, name string, data []byte) blueprintAgentFile {
	t.Helper()
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("%s: no front matter", name)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("%s: unterminated front matter", name)
	}
	var front map[string]any
	if err := yaml.Unmarshal([]byte(rest[:end+1]), &front); err != nil {
		t.Fatalf("%s: front matter: %v", name, err)
	}
	return blueprintAgentFile{front: jsonNormal(t, front).(map[string]any), body: rest[end+len("\n---\n"):], dir: path.Dir(name)}
}

// jsonNormal round-trips a value through JSON (yaml ints become float64).
func jsonNormal(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// blueprintAvatarFile is one uploaded avatar file a file names.
type blueprintAvatarFile struct {
	slot, extension string
	data            []byte
}

// avatarFiles reads the files of a slot -> path avatar map. A path is
// relative to the zip root (the team folder, or the agent's folder), and a
// source-tree member reused from ../../agents is also tried beside its file.
func avatarFiles(t *testing.T, asset blueprintAsset, file blueprintAgentFile, root string) []blueprintAvatarFile {
	t.Helper()
	paths, ok := file.front["avatar"].(map[string]any)
	if !ok {
		return nil
	}
	var files []blueprintAvatarFile
	for slot, raw := range paths {
		relative, _ := raw.(string)
		var data []byte
		var err error
		for _, base := range []string{root, file.dir} {
			if data, err = asset.read(path.Join(base, relative)); err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("%s: avatar %s (%s): %v", asset.label, slot, relative, err)
		}
		files = append(files, blueprintAvatarFile{slot: slot, extension: strings.TrimPrefix(path.Ext(relative), "."), data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].slot < files[j].slot })
	return files
}

// blueprintEntities maps one agent file into the import document's entities
// (FORMAT section 1, "Mapping into the import document"): the agent entry
// plus a skill entity for each skill block. avatarSlot answers the pack url
// of one uploaded file; the entry's avatar_media is filled when it embeds.
func blueprintEntities(uuid string, file blueprintAgentFile, pack map[string]any, media map[string]string) (map[string]any, []any) {
	front := file.front
	llm := map[string]any{}
	for from, to := range map[string]string{"model": "model_name", "temperature": "temperature", "max_tokens": "max_tokens", "top_p": "top_p", "reasoning_effort": "reasoning_effort"} {
		if value, ok := front[from]; ok {
			llm[to] = value
		}
	}
	meta := map[string]any{}
	for _, key := range append([]string{"step_limit", "internal_tools"}, blueprintRichMetaKeys...) {
		if value, ok := front[key]; ok {
			meta[key] = value
		}
	}
	version := map[string]any{
		"name": "base", "agent_type": front["agent_type"], "instructions": file.body,
		"llm_settings": llm, "meta": meta, "import_version_uuid": uuid + "-v",
	}
	if name, ok := front["version"].(string); ok && name != "" {
		version["name"] = name
	}
	for _, key := range []string{"tags", "soul", "welcome_message", "conversation_starters", "variables"} {
		if value, ok := front[key]; ok {
			version[key] = value
		}
	}
	if pack != nil {
		version["avatar_pack"] = pack
	}
	if len(media) > 0 {
		version["avatar_media"] = media
	}
	var skills []any
	var references []any
	for position, raw := range toList(front["skills"]) {
		block, _ := raw.(map[string]any)
		skillUUID := fmt.Sprintf("%s-skill-%d", uuid, position)
		versionName, _ := block["version"].(string)
		if versionName == "" {
			versionName = "base"
		}
		skills = append(skills, map[string]any{
			"entity": "skills", "import_uuid": skillUUID, "name": block["name"], "description": block["description"],
			"versions": []any{map[string]any{"name": versionName, "instructions": block["instructions"]}},
		})
		references = append(references, map[string]any{"import_uuid": skillUUID, "version_name": versionName})
	}
	if len(references) > 0 {
		version["skills"] = references
	}
	entry := map[string]any{
		"entity": "agents", "import_uuid": uuid, "name": front["name"], "description": front["description"],
		"versions": []any{version},
	}
	for _, key := range []string{"evaluation", "memory_seeds"} {
		if value, ok := front[key]; ok {
			entry[key] = value
		}
	}
	return entry, skills
}

// blueprintRichMetaKeys mirrors the format's meta blocks, in its order.
var blueprintRichMetaKeys = []string{
	"memory", "context", "proactivity", "tool_envelope",
	"capabilities", "run_caps", "output_contract", "citations",
}

func toList(value any) []any {
	list, _ := value.([]any)
	return list
}

// repositoryRoot walks up from the package to the folder holding go.work.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work above the package: cannot find the repository's blueprints/")
		}
		dir = parent
	}
}

// directoryReader reads files under root by slash path.
func directoryReader(root string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		clean := path.Clean(name)
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
	}
}

// zipReader reads the entries of a zip by slash path.
func zipReader(t *testing.T, data []byte) func(string) ([]byte, error) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	files := map[string]*zip.File{}
	for _, file := range archive.File {
		files[file.Name] = file
	}
	return func(name string) ([]byte, error) {
		file, ok := files[path.Clean(name)]
		if !ok {
			return nil, os.ErrNotExist
		}
		reader, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = reader.Close() }()
		return io.ReadAll(reader)
	}
}

// sourceBlueprintAssets lists the repository's blueprints/ tree.
func sourceBlueprintAssets(t *testing.T) []blueprintAsset {
	t.Helper()
	root := filepath.Join(repositoryRoot(t), "blueprints")
	read := directoryReader(root)
	var assets []blueprintAsset
	agents, _ := filepath.Glob(filepath.Join(root, "agents", "*.agent.md"))
	sort.Strings(agents)
	for _, file := range agents {
		assets = append(assets, blueprintAsset{label: "agents/" + filepath.Base(file), entry: "agents/" + filepath.Base(file), read: read})
	}
	teams, _ := filepath.Glob(filepath.Join(root, "teams", "*", "team.yaml"))
	sort.Strings(teams)
	for _, file := range teams {
		team := filepath.Base(filepath.Dir(file))
		assets = append(assets, blueprintAsset{label: "teams/" + team, team: true, entry: "teams/" + team + "/team.yaml", read: read})
	}
	return assets
}

// builtBlueprintAssets lists the builder's output under dir through its
// index.json, and answers how many assets the index names.
func builtBlueprintAssets(t *testing.T, dir string) ([]blueprintAsset, int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("ELITEA_BLUEPRINTS_DIR=%s: %v", dir, err)
	}
	var index struct {
		Schema string                        `json:"schema"`
		Agents []struct{ Slug, File string } `json:"agents"`
		Teams  []struct{ Slug, File string } `json:"teams"`
	}
	if err := json.Unmarshal(data, &index); err != nil || index.Schema != "elitea.blueprints/v1" {
		t.Fatalf("index.json: schema %q, %v", index.Schema, err)
	}
	var assets []blueprintAsset
	for _, agent := range index.Agents {
		bytesOf, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(agent.File)))
		if err != nil {
			t.Fatalf("index.json names %s: %v", agent.File, err)
		}
		if strings.HasSuffix(agent.File, ".zip") {
			read := zipReader(t, bytesOf)
			entry := ""
			for _, candidate := range []string{agent.Slug + ".agent.md", agent.Slug + ".pipeline.md"} {
				if _, err := read(candidate); err == nil {
					entry = candidate
				}
			}
			if entry == "" {
				t.Fatalf("%s holds no %s.agent.md", agent.File, agent.Slug)
			}
			assets = append(assets, blueprintAsset{label: agent.File, entry: entry, read: read})
			continue
		}
		assets = append(assets, blueprintAsset{label: agent.File, entry: path.Base(agent.File), read: func(name string) ([]byte, error) {
			if path.Clean(name) == path.Base(agent.File) {
				return bytesOf, nil
			}
			return nil, os.ErrNotExist
		}})
	}
	for _, team := range index.Teams {
		bytesOf, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(team.File)))
		if err != nil {
			t.Fatalf("index.json names %s: %v", team.File, err)
		}
		assets = append(assets, blueprintAsset{label: team.File, team: true, entry: "team.yaml", read: zipReader(t, bytesOf)})
	}
	return assets, len(index.Agents) + len(index.Teams)
}

// blueprintTeam is a parsed team.yaml: its fields and its member files'
// paths, relative to the team's folder.
type blueprintTeam struct {
	name, description, floorPolicy, lead string
	members                              []string
}

func parseBlueprintTeam(t *testing.T, label string, data []byte) blueprintTeam {
	t.Helper()
	var raw struct {
		Schema      string   `yaml:"schema"`
		Kind        string   `yaml:"kind"`
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		FloorPolicy string   `yaml:"floor_policy"`
		Members     []string `yaml:"members"`
		Lead        string   `yaml:"lead"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%s: team.yaml: %v", label, err)
	}
	if raw.Schema != "" && raw.Schema != "elitea.team/v1" {
		t.Fatalf("%s: team.yaml schema %q", label, raw.Schema)
	}
	if len(raw.Members) == 0 {
		t.Fatalf("%s: team.yaml names no members", label)
	}
	return blueprintTeam{name: raw.Name, description: raw.Description, floorPolicy: raw.FloorPolicy, lead: raw.Lead, members: raw.Members}
}
