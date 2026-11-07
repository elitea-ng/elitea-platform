package localturns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

type fakeComputer struct {
	upload    bool
	readErr   error
	turnErr   error
	stored    []byte
	storedFor string
	turnCred  localturn.Credential
}

func (f *fakeComputer) UploadScreenshots(context.Context, int64) (bool, error) {
	return f.upload, f.readErr
}

func (f *fakeComputer) ScreenshotTurn(_ context.Context, _, _ int64, _ string, credential localturn.Credential) (string, error) {
	f.turnCred = credential
	return "6f1c2d3e-4a5b-4c6d-8e7f-901234567890", f.turnErr
}

func (f *fakeComputer) StoreScreenshot(_ context.Context, _ int64, conversationUUID, name, _ string, content []byte) (string, error) {
	f.stored, f.storedFor = content, conversationUUID
	return "/chat-attachments/" + conversationUUID + "/" + name, nil
}

const screenshotURL = "/api/v2/elitea_core/local_turn_screenshot/prompt_lib/1/0123456789abcdef0123456789abcdef"

func screenshotJSON(t *testing.T, content []byte, digest string) string {
	t.Helper()
	if digest == "" {
		sum := sha256.Sum256(content)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	body, _ := json.Marshal(map[string]string{
		"name": "computer-screenshot-call_1.png", "media_type": "image/png", "digest": digest,
		"content": base64.StdEncoding.EncodeToString(content),
	})
	return string(body)
}

// The upload is off by default: a start without the setting answers it
// off, and the upload refuses until the project opts in (ADR-0047
// decision 9).
func TestComputerScreenshotUploadIsOffByDefault(t *testing.T) {
	for _, h := range []*handler{
		{useCase: &fakeUseCase{}},
		{useCase: &fakeUseCase{}, computer: &fakeComputer{}},
		{useCase: &fakeUseCase{}, computer: &fakeComputer{upload: true, readErr: errors.New("corrupt")}},
	} {
		response := serve(t, h.start, tokenUser(), startURL, StartPath, `{}`)
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		computer, _ := body["computer_use"].(map[string]any)
		if computer == nil || computer["upload_screenshots"] != false {
			t.Fatalf("start computer_use = %v, want upload_screenshots false", body["computer_use"])
		}
	}
	for _, h := range []*handler{{}, {computer: &fakeComputer{}}} {
		response := serve(t, h.uploadScreenshot, tokenUser(), screenshotURL, ScreenshotPath, screenshotJSON(t, []byte("png"), ""))
		if response.Code != http.StatusForbidden || !json.Valid(response.Body.Bytes()) ||
			!containsCode(response.Body.Bytes(), "computer_screenshot_upload_off") {
			t.Fatalf("upload without the opt-in = %d %s", response.Code, response.Body)
		}
	}
}

func TestComputerScreenshotUploadStoresWithTheOptIn(t *testing.T) {
	computer := &fakeComputer{upload: true}
	h := &handler{useCase: &fakeUseCase{}, computer: computer}
	start := serve(t, h.start, tokenUser(), startURL, StartPath, `{}`)
	if !containsJSON(start.Body.Bytes(), "computer_use", map[string]any{"upload_screenshots": true}) {
		t.Fatalf("start = %s", start.Body)
	}
	response := serve(t, h.uploadScreenshot, tokenUser(), screenshotURL, ScreenshotPath, screenshotJSON(t, []byte("png"), ""))
	if response.Code != http.StatusCreated || string(computer.stored) != "png" ||
		computer.turnCred != (localturn.Credential{TokenID: "70", NativeClientID: "ai.elitea.desktop"}) {
		t.Fatalf("upload = %d %s, stored %q, credential %+v", response.Code, response.Body, computer.stored, computer.turnCred)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["filepath"] != "/chat-attachments/6f1c2d3e-4a5b-4c6d-8e7f-901234567890/computer-screenshot-call_1.png" {
		t.Fatalf("filepath = %v", body["filepath"])
	}
}

func TestComputerScreenshotUploadRefusesBadBodiesAndClosedTurns(t *testing.T) {
	h := &handler{computer: &fakeComputer{upload: true}}
	for name, body := range map[string]string{
		"wrong digest": screenshotJSON(t, []byte("png"), "sha256:00"),
		"empty":        screenshotJSON(t, nil, ""),
		"bad name":     `{"name":"../x.png","media_type":"image/png","digest":"","content":""}`,
		"bad type":     `{"name":"computer-screenshot-a.png","media_type":"image/gif","digest":"","content":"cG5n"}`,
	} {
		response := serve(t, h.uploadScreenshot, tokenUser(), screenshotURL, ScreenshotPath, body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s", name, response.Code, response.Body)
		}
	}
	for err, want := range map[error]int{
		localturn.ErrNotFound: http.StatusNotFound, localturn.ErrAlreadyCommitted: http.StatusConflict,
		localturn.ErrExpired: http.StatusGone,
	} {
		h := &handler{computer: &fakeComputer{upload: true, turnErr: err}}
		response := serve(t, h.uploadScreenshot, tokenUser(), screenshotURL, ScreenshotPath, screenshotJSON(t, []byte("png"), ""))
		if response.Code != want {
			t.Errorf("%v: status = %d, want %d", err, response.Code, want)
		}
	}
}

func containsCode(raw []byte, code string) bool {
	var body map[string]any
	return json.Unmarshal(raw, &body) == nil && body["error"] == code
}

func containsJSON(raw []byte, key string, want map[string]any) bool {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	got, _ := json.Marshal(body[key])
	expected, _ := json.Marshal(want)
	return string(got) == string(expected)
}
