package agentmediaapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// poseGateway answers each edit with the image of its pose number, after a
// pause long enough for the calls to overlap, and records how many were in
// flight at once (#1854).
type poseGateway struct {
	llmproxy.Gateway
	images   map[int][]byte
	failPose int
	mu       sync.Mutex
	inFlight int
	peak     int
}

var posePattern = regexp.MustCompile(`Pose (\d+) of`)

func (g *poseGateway) ImageEdit(ctx context.Context, req *llmproxy.Request) (*llmproxy.Response, error) {
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	reader := multipart.NewReader(req.Body, params["boundary"])
	pose := 0
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "prompt" {
			if match := posePattern.FindStringSubmatch(string(data)); match != nil {
				pose, _ = strconv.Atoi(match[1])
			}
		}
	}
	g.mu.Lock()
	g.inFlight++
	if g.inFlight > g.peak {
		g.peak = g.inFlight
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()
	select {
	case <-time.After(30 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if pose == g.failPose {
		return &llmproxy.Response{StatusCode: http.StatusPaymentRequired, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	body := `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(g.images[pose]) + `"}]}`
	return &llmproxy.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestLoopFramesRunInParallelAndKeepPoseOrder(t *testing.T) {
	still := pngOf(t, 32, color.White)
	media, err := agentmedia.Inspect(still)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &poseGateway{images: map[int][]byte{}}
	for pose := 1; pose <= agentmedia.MaxLoopFrames; pose++ {
		gateway.images[pose] = pngOf(t, 16, color.NRGBA{R: uint8(pose * 20), A: 255})
	}
	h := NewHandler(newMemoryStore(), gateway)
	req := GenerateRequest{Kind: "loop", Model: "m", State: agentmedia.SlotThinking, Frames: agentmedia.MaxLoopFrames}
	frames, err := h.loopFrames(context.Background(), llmproxy.Identity{ProjectID: "7"}, req, still, media)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != agentmedia.MaxLoopFrames {
		t.Fatalf("frames = %d", len(frames))
	}
	for index, frame := range frames {
		if !bytes.Equal(frame, gateway.images[index+1]) {
			t.Fatalf("frame %d is not pose %d's image", index, index+1)
		}
	}
	if gateway.peak < 2 || gateway.peak > maxConcurrentFrameEdits {
		t.Fatalf("peak concurrent edits = %d, want 2..%d", gateway.peak, maxConcurrentFrameEdits)
	}
}

func TestLoopFrameFailureFailsTheGenerationWithItsStatus(t *testing.T) {
	store := newMemoryStore()
	gateway := &poseGateway{images: map[int][]byte{}, failPose: 3}
	for pose := 1; pose <= 6; pose++ {
		gateway.images[pose] = pngOf(t, 16, color.Black)
	}
	handler := router(NewHandler(store, gateway), store)
	still := upload(t, handler, "7", "s.png", "image/png", pngOf(t, 32, color.White))
	if still.Code != http.StatusCreated {
		t.Fatalf("upload: %d", still.Code)
	}
	stored := store.count()
	var object MediaObject
	if err := json.Unmarshal(still.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	rec := generate(t, handler, "7", map[string]any{
		"kind": "loop", "model": "m", "state": "thinking", "still_url": object.URL, "frames": 6,
	})
	// One frame's budget refusal is the request's answer, and a loop that did
	// not complete stores nothing.
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.count() != stored {
		t.Fatalf("a failed loop stored %d objects", store.count()-stored)
	}
}

// Two concurrent loop requests of one project share one budget of
// maxConcurrentFrameEdits provider calls (the errgroup limit alone bounds
// each request, so two requests ran eight).
func TestLoopFramesShareOneBoundPerProject(t *testing.T) {
	still := pngOf(t, 32, color.White)
	media, err := agentmedia.Inspect(still)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &poseGateway{images: map[int][]byte{}}
	for pose := 1; pose <= agentmedia.MaxLoopFrames; pose++ {
		gateway.images[pose] = pngOf(t, 16, color.NRGBA{R: uint8(pose * 20), A: 255})
	}
	h := NewHandler(newMemoryStore(), gateway)
	req := GenerateRequest{Kind: "loop", Model: "m", State: agentmedia.SlotThinking, Frames: agentmedia.MaxLoopFrames}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.loopFrames(context.Background(), llmproxy.Identity{ProjectID: "7"}, req, still, media); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if gateway.peak > maxConcurrentFrameEdits {
		t.Fatalf("peak concurrent edits across requests = %d, want at most %d", gateway.peak, maxConcurrentFrameEdits)
	}
}
