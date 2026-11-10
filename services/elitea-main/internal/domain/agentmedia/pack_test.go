package agentmedia

import (
	"encoding/json"
	"strings"
	"testing"
)

func mediaURL(ext string) string {
	return URLPrefix + "7/" + strings.Repeat("ab", 32) + "." + ext
}

func TestNormalizePack(t *testing.T) {
	good := map[string]any{
		"schema": PackSchema,
		"slots": map[string]any{
			"still":    map[string]any{"url": mediaURL("svg"), "media_type": "text/html"},
			"thinking": map[string]any{"url": mediaURL("webm"), "poster_url": mediaURL("png"), "generated_by": " gpt-image-1 "},
			"speaking": map[string]any{"url": mediaURL("webp"), "poster_url": mediaURL("png")},
		},
	}
	canonical, err := NormalizePack(good)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	var pack Pack
	if err := json.Unmarshal(canonical, &pack); err != nil {
		t.Fatal(err)
	}
	if pack.Slots["still"].MediaType != "image/svg+xml" {
		t.Fatalf("media type is not derived from the URL: %+v", pack.Slots["still"])
	}
	if pack.Slots["thinking"].GeneratedBy != "gpt-image-1" {
		t.Fatalf("generated_by not trimmed: %+v", pack.Slots["thinking"])
	}
	if pack.Slots["speaking"].PosterURL != "" {
		t.Fatalf("a WebP keeps a poster it does not need: %+v", pack.Slots["speaking"])
	}
	if StillURL(canonical) != mediaURL("svg") {
		t.Fatalf("StillURL = %q", StillURL(canonical))
	}
	if out, err := NormalizePack(nil); err != nil || out != nil {
		t.Fatalf("nil pack: %v %v", out, err)
	}

	bad := map[string]map[string]any{
		"wrong schema":     {"schema": "x", "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}}},
		"no still":         {"schema": PackSchema, "slots": map[string]any{"idle": map[string]any{"url": mediaURL("webp")}}},
		"video still":      {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": mediaURL("mp4"), "poster_url": mediaURL("png")}}},
		"unknown slot":     {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}, "dancing": map[string]any{"url": mediaURL("png")}}},
		"unknown key":      {"schema": PackSchema, "lottie": true, "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}}},
		"external url":     {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": "https://evil.example/a.png"}}},
		"icon url":         {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": "/icons/7/a.png"}}},
		"svg animation":    {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}, "idle": map[string]any{"url": mediaURL("svg")}}},
		"video no poster":  {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}, "idle": map[string]any{"url": mediaURL("mp4")}}},
		"video poster":     {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": mediaURL("png")}, "idle": map[string]any{"url": mediaURL("mp4"), "poster_url": mediaURL("webm")}}},
		"lottie extension": {"schema": PackSchema, "slots": map[string]any{"still": map[string]any{"url": URLPrefix + "7/" + strings.Repeat("ab", 32) + ".json"}}},
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizePack(raw); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
