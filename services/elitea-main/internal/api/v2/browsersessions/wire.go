package browsersessions

import (
	"encoding/json"
	"errors"
	"unicode/utf8"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// The view WebSocket's messages (client contract 1.40). Down, the server
// sends one JSON text message per event, and each frame's JPEG as the ONE
// binary message that follows its `frame` header. Up, the viewer sends JSON
// text messages only. v2.yaml (viewBrowserSession) is the contract.

// Down message types.
const (
	typeHello   = "hello"
	typeFrame   = "frame"
	typeNotice  = "notice"
	typeEnd     = "end"
	typeRefused = "refused"
)

// Up message types.
const (
	typeAck      = "ack"
	typePointer  = "pointer"
	typeWheel    = "wheel"
	typeKey      = "key"
	typeTakeOver = "take_over"
	typeHandBack = "hand_back"
)

// The bounds of one up message: the supervisor's own (browser_session.proto).
const (
	maxUpMessageBytes = 4 << 10
	maxKeyFieldBytes  = 32
	maxNoteBytes      = 2 << 10
)

// helloMessage opens the stream: who may drive, as Main decided it.
type helloMessage struct {
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	CanControl bool   `json:"can_control"`
	Lingering  bool   `json:"lingering"`
}

// frameMessage is the header of one screen frame; its JPEG follows as one
// binary message. `control` is "agent" or "user".
type frameMessage struct {
	Type    string `json:"type"`
	Seq     uint64 `json:"seq"`
	Width   uint32 `json:"width"`
	Height  uint32 `json:"height"`
	Bytes   int    `json:"bytes"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Control string `json:"control"`
}

// codeMessage is a notice, a refusal of one up message, or the end.
type codeMessage struct {
	Type string `json:"type"`
	Code string `json:"code"`
}

// upMessage is any viewer message; unused fields stay zero.
type upMessage struct {
	Type       string  `json:"type"`
	Seq        uint64  `json:"seq"`
	Kind       string  `json:"kind"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     uint32  `json:"button"`
	ClickCount uint32  `json:"click_count"`
	Modifiers  uint32  `json:"modifiers"`
	DeltaX     float64 `json:"delta_x"`
	DeltaY     float64 `json:"delta_y"`
	Key        string  `json:"key"`
	Code       string  `json:"code"`
	Text       string  `json:"text"`
	Note       string  `json:"note"`
}

var errBadUp = errors.New("malformed viewer message")

func controlName(control runtimev1.BrowserControlV1) string {
	if control == runtimev1.BrowserControlV1_BROWSER_CONTROL_V1_USER {
		return "user"
	}
	return "agent"
}

// decodeUp parses one viewer message into the supervisor's input frame.
// isInput reports whether it is input (pointer, wheel, key, take_over,
// hand_back), which a watch-only viewer may not send.
func decodeUp(raw []byte) (frame *runtimev1.BrowserViewInputV1, isInput bool, err error) {
	if len(raw) > maxUpMessageBytes || !utf8.Valid(raw) {
		return nil, false, errBadUp
	}
	var m upMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false, errBadUp
	}
	input := func(event *runtimev1.BrowserInputEventV1) *runtimev1.BrowserViewInputV1 {
		return &runtimev1.BrowserViewInputV1{Frame: &runtimev1.BrowserViewInputV1_Input{Input: event}}
	}
	switch m.Type {
	case typeAck:
		return &runtimev1.BrowserViewInputV1{Frame: &runtimev1.BrowserViewInputV1_Ack{Ack: m.Seq}}, false, nil
	case typePointer:
		kind, ok := map[string]runtimev1.BrowserPointerKindV1{
			"move": runtimev1.BrowserPointerKindV1_BROWSER_POINTER_KIND_V1_MOVE,
			"down": runtimev1.BrowserPointerKindV1_BROWSER_POINTER_KIND_V1_DOWN,
			"up":   runtimev1.BrowserPointerKindV1_BROWSER_POINTER_KIND_V1_UP,
		}[m.Kind]
		if !ok || m.Button > 3 || m.ClickCount > 3 || m.Modifiers > 15 || !finite(m.X, m.Y) {
			return nil, true, errBadUp
		}
		return input(&runtimev1.BrowserInputEventV1{Event: &runtimev1.BrowserInputEventV1_Pointer{Pointer: &runtimev1.BrowserPointerEventV1{
			Kind: kind, X: m.X, Y: m.Y, Button: m.Button, ClickCount: m.ClickCount, Modifiers: m.Modifiers,
		}}}), true, nil
	case typeWheel:
		if m.Modifiers > 15 || !finite(m.X, m.Y, m.DeltaX, m.DeltaY) {
			return nil, true, errBadUp
		}
		return input(&runtimev1.BrowserInputEventV1{Event: &runtimev1.BrowserInputEventV1_Wheel{Wheel: &runtimev1.BrowserWheelEventV1{
			X: m.X, Y: m.Y, DeltaX: m.DeltaX, DeltaY: m.DeltaY, Modifiers: m.Modifiers,
		}}}), true, nil
	case typeKey:
		kind, ok := map[string]runtimev1.BrowserKeyKindV1{
			"down": runtimev1.BrowserKeyKindV1_BROWSER_KEY_KIND_V1_DOWN,
			"up":   runtimev1.BrowserKeyKindV1_BROWSER_KEY_KIND_V1_UP,
			"char": runtimev1.BrowserKeyKindV1_BROWSER_KEY_KIND_V1_CHAR,
		}[m.Kind]
		if !ok || len(m.Key) > maxKeyFieldBytes || len(m.Code) > maxKeyFieldBytes || len(m.Text) > maxKeyFieldBytes || m.Modifiers > 15 {
			return nil, true, errBadUp
		}
		return input(&runtimev1.BrowserInputEventV1{Event: &runtimev1.BrowserInputEventV1_Key{Key: &runtimev1.BrowserKeyEventV1{
			Kind: kind, Key: m.Key, Code: m.Code, Text: m.Text, Modifiers: m.Modifiers,
		}}}), true, nil
	case typeTakeOver:
		return input(&runtimev1.BrowserInputEventV1{Event: &runtimev1.BrowserInputEventV1_TakeOver{TakeOver: true}}), true, nil
	case typeHandBack:
		if len(m.Note) > maxNoteBytes {
			return nil, true, errBadUp
		}
		return input(&runtimev1.BrowserInputEventV1{Event: &runtimev1.BrowserInputEventV1_HandBack{HandBack: &runtimev1.BrowserHandBackV1{Note: m.Note}}}), true, nil
	default:
		return nil, false, errBadUp
	}
}

func finite(values ...float64) bool {
	for _, v := range values {
		if v != v || v > 1e6 || v < -1e6 {
			return false
		}
	}
	return true
}
