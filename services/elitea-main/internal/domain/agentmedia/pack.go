package agentmedia

// The avatar pack (ADR-0050 decision 6): typed slots on the agent version,
// stored in application_versions.avatar_pack (tenant migration 0234). It
// replaces meta.icon_meta as the source of an agent's look; the version
// write keeps meta.icon_meta equal to the pack's still, so a client that
// reads only the old field still shows the right image.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// PackSchema is the only `schema` value of this version.
const PackSchema = "elitea.avatar_pack/v1"

// PackCode is the refusal code of a pack that fails the shape rules.
const PackCode = "avatar_pack_invalid"

// The slots. `still` is required; the others are optional.
const (
	SlotStill    = "still"
	SlotIdle     = "idle"
	SlotThinking = "thinking"
	SlotSpeaking = "speaking"
	SlotWorking  = "working"
	SlotWaiting  = "waiting"
	SlotSuccess  = "success"
	SlotError    = "error"
	SlotWaking   = "waking"
)

// Slots lists every slot, `still` first.
var Slots = []string{SlotStill, SlotIdle, SlotThinking, SlotSpeaking, SlotWorking, SlotWaiting, SlotSuccess, SlotError, SlotWaking}

// URLPrefix is the public path prefix every pack media URL has.
const URLPrefix = "/api/v2/agent_media/"

// mediaURLPattern is the only URL shape a slot takes: a project's
// content-addressed object, `/api/v2/agent_media/{project}/{sha256}.{ext}`.
var mediaURLPattern = regexp.MustCompile(`^/api/v2/agent_media/([1-9][0-9]{0,17})/([0-9a-f]{64})\.(png|jpg|webp|svg|mp4|webm)$`)

// mediaTypes maps a stored extension to the type the object is served as.
var mediaTypes = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp",
	"svg": "image/svg+xml", "mp4": "video/mp4", "webm": "video/webm",
}

// MediaTypeOf answers the served media type of a stored extension, "" for
// one this package never stores.
func MediaTypeOf(extension string) string { return mediaTypes[extension] }

// ParseURL splits a pack media URL into its project id and file name
// (`{sha256}.{ext}`).
func ParseURL(url string) (projectID, file, extension string, ok bool) {
	match := mediaURLPattern.FindStringSubmatch(url)
	if match == nil {
		return "", "", "", false
	}
	return match[1], match[2] + "." + match[3], match[3], true
}

// URLOf renders the public URL of a stored object.
func URLOf(projectID, file string) string { return URLPrefix + projectID + "/" + file }

// Slot is one slot of a pack.
type Slot struct {
	URL string `json:"url"`
	// MediaType is derived from the URL's extension on every write; a value
	// the client sends is ignored.
	MediaType string `json:"media_type"`
	// PosterURL is the still frame a video shows before it plays and under
	// reduced motion. Required for MP4 and WebM.
	PosterURL string `json:"poster_url,omitempty"`
	// GeneratedBy names the model that made the file, when it was generated.
	GeneratedBy string `json:"generated_by,omitempty"`
}

// Pack is the stored document.
type Pack struct {
	Schema string          `json:"schema"`
	Slots  map[string]Slot `json:"slots"`
}

// PackError is a refused pack. Message names the slot and the rule.
type PackError struct{ Message string }

func (e *PackError) Error() string { return e.Message }

func refusePack(format string, args ...any) error {
	return &PackError{Message: fmt.Sprintf(format, args...)}
}

const maxPackBytes = 8 << 10

// NormalizePack checks a pack as decoded from a JSON body (nil, or a map)
// and answers its canonical JSON. nil answers nil (no pack). Unknown keys
// and slots are refused, every URL must be a pack media URL, SVG is a still
// only, and a video needs a poster.
func NormalizePack(raw any) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > maxPackBytes {
		return nil, refusePack("the avatar pack must be a JSON object of at most %d bytes", maxPackBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var pack Pack
	if err := decoder.Decode(&pack); err != nil {
		return nil, refusePack("the avatar pack is not an %s document: %s", PackSchema, safeDecodeReason(err))
	}
	if pack.Schema != PackSchema {
		return nil, refusePack("the avatar pack's schema must be %q", PackSchema)
	}
	if _, ok := pack.Slots[SlotStill]; !ok {
		return nil, refusePack("the avatar pack needs a still")
	}
	known := make(map[string]bool, len(Slots))
	for _, slot := range Slots {
		known[slot] = true
	}
	names := make([]string, 0, len(pack.Slots))
	for name := range pack.Slots {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !known[name] {
			return nil, refusePack("the avatar pack has an unknown slot %q", name)
		}
		slot := pack.Slots[name]
		_, _, extension, ok := ParseURL(slot.URL)
		if !ok {
			return nil, refusePack("slot %q: the url must be an uploaded agent media path (%s{project}/{sha256}.{ext})", name, URLPrefix)
		}
		if extension == "svg" && name != SlotStill {
			return nil, refusePack("slot %q: SVG is accepted for the still only", name)
		}
		slot.MediaType = mediaTypes[extension]
		video := extension == "mp4" || extension == "webm"
		if video && name == SlotStill {
			return nil, refusePack("slot %q: the still must be a PNG, JPEG, WebP or SVG image", name)
		}
		if slot.PosterURL != "" {
			_, _, posterExt, ok := ParseURL(slot.PosterURL)
			if !ok || posterExt == "svg" || posterExt == "mp4" || posterExt == "webm" {
				return nil, refusePack("slot %q: the poster must be an uploaded PNG, JPEG or WebP", name)
			}
		}
		if video && slot.PosterURL == "" {
			return nil, refusePack("slot %q: a video needs a poster frame", name)
		}
		if !video {
			slot.PosterURL = ""
		}
		slot.GeneratedBy = strings.TrimSpace(slot.GeneratedBy)
		if len(slot.GeneratedBy) > 256 {
			return nil, refusePack("slot %q: generated_by is longer than 256 bytes", name)
		}
		pack.Slots[name] = slot
	}
	canonical, err := json.Marshal(pack)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

// StillURL reads the still's URL out of a stored pack; "" when there is none.
func StillURL(pack json.RawMessage) string {
	var decoded Pack
	if len(pack) == 0 || json.Unmarshal(pack, &decoded) != nil {
		return ""
	}
	return decoded.Slots[SlotStill].URL
}

// safeDecodeReason keeps the decoder's rule and drops any echoed value.
func safeDecodeReason(err error) string {
	text := err.Error()
	if strings.Contains(text, "unknown field") {
		return text
	}
	return "a key has the wrong type"
}
