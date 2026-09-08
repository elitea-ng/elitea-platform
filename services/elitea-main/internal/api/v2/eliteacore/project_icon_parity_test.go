package eliteacore_test

// The three ways the project-icon routes were NARROWER than the contract
// legacy/plugins/elitea_core/api/v2/project_icon.py serves, found while
// describing the family in api/openapi/v2.yaml (issue 36, item 5).
//
// Each test below fails against the code as it stood before this change, and
// each fails on a 200: the listing answered page one and called it the whole
// set, the upload accepted a file of any size, and it answered two of the five
// icon_meta keys the picker stores. None of that is a crash, which is why it
// survived a test suite that only asserted 2xx.

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// projectIconUploadRequest is iconUploadRequest with the two form fields the
// picker sends beside the file. iconUploadRequest cannot carry them, and the
// dimensions are half of what this file measures.
func projectIconUploadRequest(
	t *testing.T,
	projectID, filename string,
	content []byte,
	form map[string]string,
) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	for field, value := range form {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatalf("write field %s: %v", field, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	request := newRequest(http.MethodPost, "/", map[string]string{"projectID": projectID}, &buf)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

// uploadProjectIcons stores n icons and returns their names in the order the
// listing must report them.
func uploadProjectIcons(t *testing.T, handler *eliteacore.Handler, count int) []string {
	t.Helper()
	names := make([]string, 0, count)
	for index := 0; index < count; index++ {
		recorder := httptest.NewRecorder()
		handler.CreateProjectIcon(
			recorder,
			projectIconUploadRequest(t, "7", fmt.Sprintf("icon%d.png", index), []byte("PNGDATA"), nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("create %d status = %d; body=%s", index, recorder.Code, recorder.Body.String())
		}
		name, _ := decodeObj(t, recorder)["name"].(string)
		if name == "" {
			t.Fatalf("create %d returned no name", index)
		}
		names = append(names, name)
	}
	return names
}

func listProjectIcons(t *testing.T, handler *eliteacore.Handler, query string) (rows []any, total float64) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ListProjectIcons(
		recorder,
		newRequest(http.MethodGet, "/"+query, map[string]string{"projectID": "7"}, nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	body := decodeObj(t, recorder)
	rows, _ = body["rows"].([]any)
	total, _ = body["total"].(float64)
	return rows, total
}

func projectIconRowNames(t *testing.T, rows []any) []string {
	t.Helper()
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		object, _ := row.(map[string]any)
		name, _ := object["name"].(string)
		names = append(names, name)
	}
	return names
}

// TestListProjectIconsHonoursSkipAndLimit
//
// RED before this change: both parameters were accepted and discarded, so
// every request answered the whole set however the caller paged. The picker
// sends `limit=200&skip=<page*200>`; a project past its first page could never
// reach the rest of its icons, and nothing said so.
func TestListProjectIconsHonoursSkipAndLimit(t *testing.T) {
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(newListingIconStore()))
	stored := uploadProjectIcons(t, handler, 5)

	// The listing is sorted by name before it is sliced, exactly as
	// social/rpc/icons.py:get_icons_list does, so a page is stable.
	sorted := append([]string(nil), stored...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	rows, total := listProjectIcons(t, handler, "?skip=0&limit=2")
	if len(rows) != 2 {
		t.Fatalf("limit=2 returned %d rows — the limit was discarded", len(rows))
	}
	if total != 5 {
		t.Fatalf("total = %v, want 5 — total counts every icon, not the page", total)
	}
	if got := projectIconRowNames(t, rows); got[0] != sorted[0] || got[1] != sorted[1] {
		t.Fatalf("page one = %v, want %v", got, sorted[:2])
	}

	next, _ := listProjectIcons(t, handler, "?skip=2&limit=2")
	if got := projectIconRowNames(t, next); len(got) != 2 || got[0] != sorted[2] || got[1] != sorted[3] {
		t.Fatalf("page two = %v, want %v", got, sorted[2:4])
	}

	// Past the end is an empty page, never a panic and never the first page
	// again — Python's forgiving slice, which pageOfIconRows reproduces.
	past, pastTotal := listProjectIcons(t, handler, "?skip=99&limit=2")
	if len(past) != 0 {
		t.Fatalf("skip past the end returned %d rows", len(past))
	}
	if pastTotal != 5 {
		t.Fatalf("total on an empty page = %v, want 5", pastTotal)
	}

	// A missing or unparseable value is pylon's default, not a 500.
	whole, _ := listProjectIcons(t, handler, "?skip=&limit=notanumber")
	if len(whole) != 5 {
		t.Fatalf("default page returned %d rows, want all 5", len(whole))
	}
}

// TestCreateProjectIconRefusesAnOversizedFile
//
// RED before this change: there was NO cap. ParseMultipartForm's argument is a
// memory budget, not a limit, and its error was discarded, so a file of any
// size was streamed into the object store under a permission a project editor
// holds. pylon answers 400 "File size exceeds 512 KB".
func TestCreateProjectIconRefusesAnOversizedFile(t *testing.T) {
	store := newListingIconStore()
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	oversized := bytes.Repeat([]byte("A"), 512*1024+1)
	recorder := httptest.NewRecorder()
	handler.CreateProjectIcon(recorder, projectIconUploadRequest(t, "7", "big.png", oversized, nil))

	if recorder.Code == http.StatusOK {
		t.Fatalf("status = 200 — a %d byte icon was accepted; body=%s",
			len(oversized), recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if code := decodeCode(t, recorder); code != "icon_too_large" {
		t.Fatalf("code = %q, want icon_too_large", code)
	}

	// Nothing was left behind: an object stored and then refused is a leak the
	// listing would advertise.
	rows, total := listProjectIcons(t, handler, "")
	if len(rows) != 0 || total != 0 {
		t.Fatalf("listing after the refusal = %d rows / total %v — the refused bytes were kept",
			len(rows), total)
	}
}

// TestCreateProjectIconAnswersTheWholeIconMeta
//
// RED before this change: the response carried `name` and `url` only.
// social/rpc/process_image.py:save_image returns five keys, the project-info
// PUT stores whatever the picker hands back, and the settings dialog renders
// the two sizes — so the stored icon_meta no longer described the file and the
// dialog showed blanks.
func TestCreateProjectIconAnswersTheWholeIconMeta(t *testing.T) {
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(newListingIconStore()))

	content := []byte("PNGDATA")
	recorder := httptest.NewRecorder()
	handler.CreateProjectIcon(
		recorder,
		projectIconUploadRequest(t, "7", "logo.png", content, map[string]string{
			"width": "128", "height": "128",
		}),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	body := decodeObj(t, recorder)

	for _, key := range []string{"name", "url", "size", "initial_file_size", "resulting_file_size"} {
		if _, present := body[key]; !present {
			t.Fatalf("%q missing from the upload response %v — save_image returns all five", key, body)
		}
	}
	if body["size"] != "128x128" {
		t.Fatalf("size = %v, want 128x128 — the requested box was discarded", body["size"])
	}
	if body["initial_file_size"] != "7.0B" {
		t.Fatalf("initial_file_size = %v, want 7.0B", body["initial_file_size"])
	}
	if body["resulting_file_size"] != "7.0B" {
		t.Fatalf("resulting_file_size = %v, want 7.0B", body["resulting_file_size"])
	}
	if name, _ := body["name"].(string); !strings.HasPrefix(name, "pi_") {
		t.Fatalf("name = %v — the project prefix keeps the two icon families apart", body["name"])
	}
}

// TestCreateProjectIconClampsTheRequestedBox pins project_icon.py's
// MAX_DIMENSION, which is 512 and NOT the 64 the agent and skill icons use.
func TestCreateProjectIconClampsTheRequestedBox(t *testing.T) {
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(newListingIconStore()))

	for _, testCase := range []struct{ width, height, want string }{
		{"9000", "9000", "512x512"},
		{"0", "-4", "1x1"},
		{"", "", "64x64"},
		{"256", "128", "256x128"},
	} {
		recorder := httptest.NewRecorder()
		form := map[string]string{}
		if testCase.width != "" {
			form["width"] = testCase.width
			form["height"] = testCase.height
		}
		handler.CreateProjectIcon(
			recorder,
			projectIconUploadRequest(t, "7", "logo.png", []byte("PNGDATA"), form),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d for %v", recorder.Code, testCase)
		}
		if size := decodeObj(t, recorder)["size"]; size != testCase.want {
			t.Fatalf("size = %v for width=%q height=%q, want %s",
				size, testCase.width, testCase.height, testCase.want)
		}
	}
}
