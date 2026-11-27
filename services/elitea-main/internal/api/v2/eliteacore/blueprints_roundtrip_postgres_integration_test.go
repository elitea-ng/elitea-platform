package eliteacore_test

// Every blueprint imports through the real routes and survives the markdown
// export (issue 3137). For each asset — a single agent through POST
// import_wizard, a team through POST team_import — the test imports it on a
// fresh database, exports every agent it created with ?format=md through the
// real route, reads the export back with the same reader, and compares the
// rich fields: schema, the model settings with reasoning_effort, tags, soul,
// every meta block, the avatar (the default key, or every slot's bytes),
// evaluation datasets, skills and the instructions. memory_seeds are never
// exported, so the test asserts they landed as pending entries written by
// the importing person instead.
//
// Avatar files take both paths the server has: a single agent's are embedded
// in the document (`avatar_media`, `{sha256}.{ext}` names, pack urls of
// another project); a team's are uploaded first through the agent media
// upload route into the target project and named by their returned url,
// which is what the web client does to keep a team import's body small (an
// eight-member team with nine animated states each is ~46 MB as base64).
//
// Source: ELITEA_BLUEPRINTS_DIR (the builder's output, every asset as it
// ships; each one index.json lists must import), else the repository's
// blueprints/ tree. Fewer than one agent fails the test, so it cannot pass
// over an empty tree.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
)

// blueprintImported is one agent the import created, with the file it came from.
type blueprintImported struct {
	appID  string
	source blueprintAgentFile
	files  []blueprintAvatarFile
}

func TestBlueprintsImportRoundTrip(t *testing.T) {
	var assets []blueprintAsset
	listed := -1
	if dir := os.Getenv("ELITEA_BLUEPRINTS_DIR"); dir != "" {
		assets, listed = builtBlueprintAssets(t, dir)
	} else {
		assets = sourceBlueprintAssets(t)
	}
	imported, agents := 0, 0
	for _, asset := range assets {
		t.Run(asset.label, func(t *testing.T) {
			agents += roundTripBlueprint(t, asset)
			imported++
		})
	}
	if agents < 1 {
		t.Fatalf("the blueprints hold %d agents; at least one is required, so this test cannot pass over an empty tree", agents)
	}
	if listed >= 0 && imported != listed {
		t.Fatalf("index.json lists %d assets and %d were imported", listed, imported)
	}
}

// blueprintRouter serves the import, team import, export and media upload
// routes as the caller importLinkPrincipal.
func blueprintRouter(handler *eliteacore.Handler, media *agentmediaapi.Handler) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			user := auth.User{ID: strconv.Itoa(importLinkPrincipal)}
			next.ServeHTTP(w, request.WithContext(auth.ContextWithUser(request.Context(), user)))
		})
	})
	router.Post("/elitea_core/import_wizard/prompt_lib/{projectID}", handler.ExportImportPost)
	router.Post("/elitea_core/team_import/prompt_lib/{projectID}", handler.TeamImportPost)
	router.Get("/elitea_core/export_import/prompt_lib/{projectID}/{entityID}", handler.ExportImportGet)
	router.Post("/elitea_core/agent_media/prompt_lib/{projectID}", media.Upload)
	return router
}

// roundTripBlueprint imports one asset on a fresh database and checks every
// agent it created; it answers how many agents that was.
func roundTripBlueprint(t *testing.T, asset blueprintAsset) int {
	pool := newImportLinkPool(t)
	store := newMediaStore()
	router := blueprintRouter(eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store)), agentmediaapi.NewHandler(store, nil))

	var agents []blueprintImported
	if asset.team {
		agents = importBlueprintTeam(t, router, asset)
	} else {
		agents = importBlueprintAgent(t, router, asset)
	}
	for _, agent := range agents {
		checkBlueprintExport(t, router, agent)
		checkMemorySeeds(t, pool, agent)
	}
	return len(agents)
}

// blueprintAvatarPack builds the pack and the embedded media of one file.
// upload true sends each file through the upload route (a team); false
// embeds it under a pack url of another project (a single agent).
func blueprintAvatarPack(t *testing.T, router chi.Router, avatar any, files []blueprintAvatarFile, upload bool) (map[string]any, map[string]string) {
	t.Helper()
	if key, ok := avatar.(string); ok && key != "" {
		return map[string]any{"schema": agentmedia.PackSchema, "slots": map[string]any{"still": map[string]any{"url": key}}}, nil
	}
	if len(files) == 0 {
		return nil, nil
	}
	slots := map[string]any{}
	media := map[string]string{}
	for _, file := range files {
		var url string
		if upload {
			url = uploadBlueprintMedia(t, router, file.data)
		} else {
			sum := sha256.Sum256(file.data)
			name := hex.EncodeToString(sum[:]) + "." + file.extension
			media[name] = base64.StdEncoding.EncodeToString(file.data)
			url = agentmedia.URLOf("9", name)
		}
		slots[file.slot] = map[string]any{"url": url}
	}
	return map[string]any{"schema": agentmedia.PackSchema, "slots": slots}, media
}

// uploadBlueprintMedia stores one file through POST agent_media into project
// 1 and answers its url.
func uploadBlueprintMedia(t *testing.T, router chi.Router, data []byte) string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "avatar")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/elitea_core/agent_media/prompt_lib/1", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload an avatar file: %d %s", recorder.Code, recorder.Body)
	}
	var object struct{ URL string }
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil || object.URL == "" {
		t.Fatalf("upload answer %s: %v", recorder.Body, err)
	}
	return object.URL
}

// blueprintImportAnswer is the part of an import answer the test reads.
type blueprintImportAnswer struct {
	Result struct {
		Agents []struct{ ID, Name string } `json:"agents"`
	} `json:"result"`
	Errors json.RawMessage `json:"errors"`
}

func importBlueprintAgent(t *testing.T, router chi.Router, asset blueprintAsset) []blueprintImported {
	t.Helper()
	data, err := asset.read(asset.entry)
	if err != nil {
		t.Fatalf("read %s: %v", asset.entry, err)
	}
	file := parseBlueprintAgent(t, asset.entry, data)
	files := avatarFiles(t, asset, file, path.Dir(asset.entry))
	pack, media := blueprintAvatarPack(t, router, file.front["avatar"], files, false)
	entry, skills := blueprintEntities("agent-1", file, pack, media)
	document := map[string]any{"applications": []any{entry}, "skills": skills}
	recorder := teamImportPost(t, router, "/elitea_core/import_wizard/prompt_lib/1", document)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import %s: %d %s", asset.label, recorder.Code, recorder.Body)
	}
	var answer blueprintImportAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || len(answer.Result.Agents) != 1 {
		t.Fatalf("import answer %s: %v", recorder.Body, err)
	}
	return []blueprintImported{{appID: answer.Result.Agents[0].ID, source: file, files: files}}
}

func importBlueprintTeam(t *testing.T, router chi.Router, asset blueprintAsset) []blueprintImported {
	t.Helper()
	data, err := asset.read(asset.entry)
	if err != nil {
		t.Fatalf("read %s: %v", asset.entry, err)
	}
	team := parseBlueprintTeam(t, asset.label, data)
	root := path.Dir(asset.entry)
	var applications, skills []any
	var members []string
	lead := ""
	byName := map[string]blueprintImported{}
	for position, member := range team.members {
		name := path.Join(root, member)
		memberData, err := asset.read(name)
		if err != nil {
			t.Fatalf("%s: member %s: %v", asset.label, member, err)
		}
		file := parseBlueprintAgent(t, name, memberData)
		files := avatarFiles(t, asset, file, root)
		pack, media := blueprintAvatarPack(t, router, file.front["avatar"], files, true)
		uuid := fmt.Sprintf("member-%d", position)
		entry, memberSkills := blueprintEntities(uuid, file, pack, media)
		applications = append(applications, entry)
		skills = append(skills, memberSkills...)
		members = append(members, uuid)
		if member == team.lead {
			lead = uuid
		}
		agentName, _ := file.front["name"].(string)
		byName[agentName] = blueprintImported{source: file, files: files}
	}
	if team.lead != "" && lead == "" {
		t.Fatalf("%s: lead %s is not a member", asset.label, team.lead)
	}
	document := map[string]any{
		"team": map[string]any{
			"name": team.name, "description": team.description, "floor_policy": team.floorPolicy,
			"lead": lead, "members": members,
		},
		"applications": applications, "skills": skills,
	}
	recorder := teamImportPost(t, router, "/elitea_core/team_import/prompt_lib/1", document)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("team import %s: %d %s", asset.label, recorder.Code, recorder.Body)
	}
	var answer blueprintImportAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || len(answer.Result.Agents) != len(team.members) {
		t.Fatalf("team import answer %s: %v", recorder.Body, err)
	}
	imported := make([]blueprintImported, 0, len(answer.Result.Agents))
	for _, agent := range answer.Result.Agents {
		source, ok := byName[agent.Name]
		if !ok {
			t.Fatalf("the team import created %q, which no member file names", agent.Name)
		}
		source.appID = agent.ID
		imported = append(imported, source)
	}
	return imported
}

// checkBlueprintExport exports one agent as markdown and compares it with
// the file it was imported from.
func checkBlueprintExport(t *testing.T, router chi.Router, agent blueprintImported) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/elitea_core/export_import/prompt_lib/1/"+agent.appID+"?format=md", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export %s: %d %s", agent.appID, recorder.Code, recorder.Body)
	}
	label, _ := agent.source.front["name"].(string)
	markdown := recorder.Body.Bytes()
	var archiveFiles map[string][]byte
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/zip") {
		archiveFiles = unzipAll(t, markdown)
		markdown = nil
		for name, data := range archiveFiles {
			if strings.HasSuffix(name, ".md") {
				markdown = data
			}
		}
		if !strings.HasSuffix(recorder.Header().Get("Content-Disposition"), `.agent.zip"`) {
			t.Errorf("%s: a single version with media is named %q, want <slug>.agent.zip", label, recorder.Header().Get("Content-Disposition"))
		}
	}
	exported := parseBlueprintAgent(t, label+" (export)", markdown)
	compareBlueprintFront(t, label, agent.source, exported)
	compareBlueprintAvatar(t, label, agent, exported, archiveFiles)
	if strings.TrimSpace(exported.body) != strings.TrimSpace(agent.source.body) {
		t.Errorf("%s: the instructions changed in the round trip", label)
	}
}

// compareBlueprintFront compares every key the format round-trips.
func compareBlueprintFront(t *testing.T, label string, source, exported blueprintAgentFile) {
	t.Helper()
	if exported.front["schema"] != "elitea.agent/v1" {
		t.Errorf("%s: exported schema = %v", label, exported.front["schema"])
	}
	if _, leaked := exported.front["memory_seeds"]; leaked {
		t.Errorf("%s: the export carries memory_seeds", label)
	}
	if _, leaked := exported.front["blueprint"]; leaked {
		t.Errorf("%s: the export carries blueprint metadata", label)
	}
	keys := append([]string{
		"name", "description", "version", "model", "temperature", "max_tokens", "top_p", "reasoning_effort",
		"step_limit", "internal_tools", "welcome_message", "conversation_starters", "soul", "variables", "evaluation",
	}, blueprintRichMetaKeys...)
	for _, key := range keys {
		if !reflect.DeepEqual(source.front[key], exported.front[key]) {
			t.Errorf("%s: %s = %v after the round trip, want %v", label, key, exported.front[key], source.front[key])
		}
	}
	wantType, _ := source.front["agent_type"].(string)
	if wantType == "" || wantType == "openai" {
		wantType = "agent"
	}
	if exported.front["agent_type"] != wantType {
		t.Errorf("%s: agent_type = %v, want %s", label, exported.front["agent_type"], wantType)
	}
	if want, got := sortedStrings(source.front["tags"]), sortedStrings(exported.front["tags"]); !reflect.DeepEqual(want, got) {
		t.Errorf("%s: tags = %v, want %v", label, got, want)
	}
	if want, got := skillSummary(source.front["skills"]), skillSummary(exported.front["skills"]); !reflect.DeepEqual(want, got) {
		t.Errorf("%s: skills = %v, want %v", label, got, want)
	}
}

// compareBlueprintAvatar compares the avatar: the same default key, or every
// slot with the same bytes inside the export's ZIP.
func compareBlueprintAvatar(t *testing.T, label string, agent blueprintImported, exported blueprintAgentFile, archive map[string][]byte) {
	t.Helper()
	if key, ok := agent.source.front["avatar"].(string); ok {
		if exported.front["avatar"] != key {
			t.Errorf("%s: avatar = %v, want %s", label, exported.front["avatar"], key)
		}
		return
	}
	if len(agent.files) == 0 {
		if exported.front["avatar"] != nil {
			t.Errorf("%s: an agent with no avatar exported %v", label, exported.front["avatar"])
		}
		return
	}
	paths, _ := exported.front["avatar"].(map[string]any)
	if len(paths) != len(agent.files) {
		t.Fatalf("%s: the export names %d avatar slots, want %d", label, len(paths), len(agent.files))
	}
	for _, file := range agent.files {
		relative, _ := paths[file.slot].(string)
		if data, ok := archive[relative]; !ok || !bytes.Equal(data, file.data) {
			t.Errorf("%s: avatar slot %s (%s) is missing from the export or changed", label, file.slot, relative)
		}
	}
}

// checkMemorySeeds asserts each memory seed landed as a pending entry
// written by the importing person.
func checkMemorySeeds(t *testing.T, pool *pgxpool.Pool, agent blueprintImported) {
	t.Helper()
	seeds := toList(agent.source.front["memory_seeds"])
	rows, err := pool.Query(t.Context(), `
		SELECT kind, content, status, written_by_kind, COALESCE(written_by_user_id, 0)
		FROM p_1.agent_memory_entries WHERE application_id = $1 ORDER BY id`, importLinkAtoi(t, agent.appID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, content, status, writer string
		var writerID int
		if err := rows.Scan(&kind, &content, &status, &writer, &writerID); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || writer != "person" || writerID != importLinkPrincipal {
			t.Errorf("seed %q: status %s, written by %s %d; want pending, person %d", content, status, writer, writerID, importLinkPrincipal)
		}
		got = append(got, kind+": "+content)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, raw := range seeds {
		seed, _ := raw.(map[string]any)
		want = append(want, fmt.Sprintf("%v: %v", seed["kind"], strings.TrimSpace(fmt.Sprint(seed["content"]))))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("memory entries = %v, want the seeds %v", got, want)
	}
}

func sortedStrings(value any) []string {
	var out []string
	for _, item := range toList(value) {
		out = append(out, fmt.Sprint(item))
	}
	sort.Strings(out)
	return out
}

func skillSummary(value any) []string {
	var out []string
	for _, item := range toList(value) {
		block, _ := item.(map[string]any)
		out = append(out, fmt.Sprintf("%v|%v|%v", block["name"], block["description"], strings.TrimSpace(fmt.Sprint(block["instructions"]))))
	}
	sort.Strings(out)
	return out
}

func unzipAll(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open the export zip: %v", err)
	}
	files := map[string][]byte{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		_, err = buffer.ReadFrom(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = buffer.Bytes()
	}
	return files
}
