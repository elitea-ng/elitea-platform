package conversations_test

// #1059: a second upload of a name already stored in the conversation used to
// overwrite the first object — bytes an earlier message still names. Each
// upload now reserves a free key, numbering the name when it is taken.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"
)

func uploadedFilepath(t *testing.T, code int, body []byte) string {
	t.Helper()
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", code, body)
	}
	var got []struct {
		Filepath string `json:"filepath"`
	}
	if err := json.Unmarshal(body, &got); err != nil || len(got) != 1 {
		t.Fatalf("decode response: %v; body=%s", err, body)
	}
	return got[0].Filepath
}

func TestAttachmentSameNameUploadKeepsTheFirstObject(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	first := doAttachmentUpload(t, h, nil, "photo.jpg", []byte("first bytes"))
	firstPath := uploadedFilepath(t, first.Code, first.Body.Bytes())
	second := doAttachmentUpload(t, h, nil, "photo.jpg", []byte("second bytes"))
	secondPath := uploadedFilepath(t, second.Code, second.Body.Bytes())
	third := doAttachmentUpload(t, h, nil, "photo.jpg", []byte("third bytes"))
	thirdPath := uploadedFilepath(t, third.Code, third.Body.Bytes())

	if firstPath != "/chat-attachments/conv-abc/photo.jpg" {
		t.Fatalf("first filepath = %q, want the plain name", firstPath)
	}
	if secondPath != "/chat-attachments/conv-abc/photo (1).jpg" || thirdPath != "/chat-attachments/conv-abc/photo (2).jpg" {
		t.Fatalf("filepaths = %q, %q; want numbered names", secondPath, thirdPath)
	}
	objStore.mu.Lock()
	defer objStore.mu.Unlock()
	if got := string(objStore.objects["conv-abc/photo.jpg"]); got != "first bytes" {
		t.Fatalf("first object = %q, want its own bytes unchanged", got)
	}
	if got := string(objStore.objects["conv-abc/photo (1).jpg"]); got != "second bytes" {
		t.Fatalf("second object = %q", got)
	}
}

func TestAttachmentNumberingKeepsTheExtensionAndHandlesDotfiles(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	for _, tc := range []struct{ name, want string }{
		{"archive.tar.gz", "/chat-attachments/conv-abc/archive.tar (1).gz"},
		{"README", "/chat-attachments/conv-abc/README (1)"},
		{".env", "/chat-attachments/conv-abc/.env (1)"},
	} {
		first := doAttachmentUpload(t, h, nil, tc.name, []byte("a"))
		uploadedFilepath(t, first.Code, first.Body.Bytes())
		second := doAttachmentUpload(t, h, nil, tc.name, []byte("b"))
		if got := uploadedFilepath(t, second.Code, second.Body.Bytes()); got != tc.want {
			t.Errorf("%s: second filepath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAttachmentConcurrentSameNameUploadsNeverShareAKey(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	const uploads = 8
	paths := make([]string, uploads)
	var wg sync.WaitGroup
	for i := range uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doAttachmentUpload(t, h, nil, "same.txt", []byte{byte('a' + i)})
			paths[i] = uploadedFilepath(t, rec.Code, rec.Body.Bytes())
		}()
	}
	wg.Wait()
	sort.Strings(paths)
	for i := 1; i < len(paths); i++ {
		if paths[i] == paths[i-1] {
			t.Fatalf("two uploads got one filepath %q: %v", paths[i], paths)
		}
	}
	objStore.mu.Lock()
	defer objStore.mu.Unlock()
	if len(objStore.objects) != uploads {
		t.Fatalf("stored objects = %d, want %d", len(objStore.objects), uploads)
	}
}

func TestAttachmentFailedPutReleasesTheReservedName(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	objStore.putErr = errors.New("store is down")
	failed := doAttachmentUpload(t, h, nil, "notes.txt", []byte("x"))
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", failed.Code)
	}
	objStore.putErr = nil
	rec := doAttachmentUpload(t, h, nil, "notes.txt", []byte("y"))
	if got := uploadedFilepath(t, rec.Code, rec.Body.Bytes()); got != "/chat-attachments/conv-abc/notes.txt" {
		t.Fatalf("filepath after a failed upload = %q, want the plain name back", got)
	}
}
