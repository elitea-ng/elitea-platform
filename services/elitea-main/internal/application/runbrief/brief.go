// Package runbrief holds the run brief and the goal check of an unattended
// chain (ADR-0045 decision 11, #1564).
//
// When a chain starts and its grant asks for a goal check, the small model
// writes a brief from the person's request: the goal, the conditions that
// show the work is done (done_when), the constraints, the assumptions and
// the ambiguity. Main validates the brief, bounds every field and stores it
// once (frozen). The first run does not wait for the brief.
//
// When a run ends and the chain would end completed, the small model reads
// the brief and the chain's transcript (32 KiB, newest content kept) and
// answers met, not_met or impossible for each condition. Main decides the
// outcome (Decide): met ends the chain completed, not_met starts a
// goal_not_met continuation, impossible or the same condition not_met on 3
// consecutive checks ends it failed, and a model failure ends it completed
// as "not checked".
//
// The brief and the check are data from a model. Main bounds every field and
// renders it as text. A brief never adds a tool, a grant answer or an
// approval. The person's request stays the first message and wins on
// conflict.
package runbrief

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds of a brief (ADR-0045 decision 11), in characters.
const (
	MaxGoalChars       = 300
	MaxConditionChars  = 300
	MaxConditions      = 5
	MaxListItemChars   = 200
	MaxListItems       = 5
	MaxReasonChars     = 300
	MaxRequestBytes    = 32 << 10
	MaxTranscriptBytes = 32 << 10
)

// Ambiguity levels.
const (
	AmbiguityLow    = "low"
	AmbiguityMedium = "medium"
	AmbiguityHigh   = "high"
)

// Condition is one done_when condition.
type Condition struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Brief is a validated run brief.
type Brief struct {
	Goal        string      `json:"goal"`
	DoneWhen    []Condition `json:"done_when"`
	Constraints []string    `json:"constraints"`
	Assumptions []string    `json:"assumptions"`
	Ambiguity   string      `json:"ambiguity"`
}

// HasConditions reports whether the brief has anything to check.
func (b *Brief) HasConditions() bool { return b != nil && len(b.DoneWhen) > 0 }

// ErrInvalidBrief is a model answer that is not a valid brief.
var ErrInvalidBrief = errors.New("runbrief: the answer is not a valid brief")

// rawBrief is the model's answer before validation.
type rawBrief struct {
	Goal     *string `json:"goal"`
	DoneWhen []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"done_when"`
	Constraints []string `json:"constraints"`
	Assumptions []string `json:"assumptions"`
	Ambiguity   string   `json:"ambiguity"`
}

// ParseBrief validates a model's answer. A fenced code block around the
// object is accepted. Text over its bound is cut; a list over its bound
// keeps its first items; condition ids become c1..cN in order. A missing
// goal, an empty condition, more than MaxConditions conditions or an
// unknown ambiguity is ErrInvalidBrief.
func ParseBrief(answer string) (Brief, error) {
	var raw rawBrief
	if err := decodeObject(answer, &raw); err != nil {
		return Brief{}, fmt.Errorf("%w: %v", ErrInvalidBrief, err)
	}
	if raw.Goal == nil {
		return Brief{}, fmt.Errorf("%w: no goal", ErrInvalidBrief)
	}
	brief := Brief{
		Goal:        Clean(*raw.Goal, MaxGoalChars),
		DoneWhen:    []Condition{},
		Constraints: cleanList(raw.Constraints),
		Assumptions: cleanList(raw.Assumptions),
		Ambiguity:   strings.ToLower(strings.TrimSpace(raw.Ambiguity)),
	}
	if brief.Goal == "" {
		return Brief{}, fmt.Errorf("%w: empty goal", ErrInvalidBrief)
	}
	if len(raw.DoneWhen) > MaxConditions {
		return Brief{}, fmt.Errorf("%w: more than %d conditions", ErrInvalidBrief, MaxConditions)
	}
	for index, condition := range raw.DoneWhen {
		text := Clean(condition.Text, MaxConditionChars)
		if text == "" {
			return Brief{}, fmt.Errorf("%w: an empty condition", ErrInvalidBrief)
		}
		brief.DoneWhen = append(brief.DoneWhen, Condition{ID: fmt.Sprintf("c%d", index+1), Text: text})
	}
	switch brief.Ambiguity {
	case AmbiguityLow, AmbiguityMedium, AmbiguityHigh:
	default:
		return Brief{}, fmt.Errorf("%w: ambiguity %q", ErrInvalidBrief, brief.Ambiguity)
	}
	return brief, nil
}

// Valid reports whether a stored brief keeps the bounds (a read guard: a
// stored brief was validated before it was written).
func (b Brief) Valid() bool {
	if b.Goal == "" || utf8.RuneCountInString(b.Goal) > MaxGoalChars || len(b.DoneWhen) > MaxConditions ||
		len(b.Constraints) > MaxListItems || len(b.Assumptions) > MaxListItems {
		return false
	}
	for index, condition := range b.DoneWhen {
		if condition.ID != fmt.Sprintf("c%d", index+1) || condition.Text == "" ||
			utf8.RuneCountInString(condition.Text) > MaxConditionChars {
			return false
		}
	}
	return true
}

// Digest is the stored form of a brief and its SHA-256 (hex).
func Digest(brief Brief) ([]byte, string, error) {
	raw, err := json.Marshal(brief)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

func cleanList(items []string) []string {
	out := []string{}
	for _, item := range items {
		if len(out) == MaxListItems {
			break
		}
		if text := Clean(item, MaxListItemChars); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// Clean is model text as Main stores it: control characters become spaces,
// runs of white space become one space, and the text is cut to limit
// characters.
func Clean(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > limit {
		text = strings.TrimSpace(string([]rune(text)[:limit]))
	}
	return text
}

// decodeObject reads one JSON object from a model's answer: the first JSON
// value from the first "{" of a fenced code block, else of the whole answer.
// Text after the object (prose, another block) is ignored.
func decodeObject(answer string, out any) error {
	text := strings.TrimSpace(answer)
	if fenced := fencedBlock(text); fenced != "" && strings.Contains(fenced, "{") {
		text = fenced
	}
	start := strings.Index(text, "{")
	if start < 0 {
		return errors.New("no JSON object")
	}
	return json.NewDecoder(strings.NewReader(text[start:])).Decode(out)
}

func fencedBlock(text string) string {
	start := strings.Index(text, "```")
	if start < 0 {
		return ""
	}
	rest := text[start+3:]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		rest = rest[newline+1:]
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// cutBytesKeepEnd keeps the last limit bytes of text, at a rune boundary.
func cutBytesKeepEnd(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}

// cutBytesKeepStart keeps the first limit bytes of text, at a rune boundary.
func cutBytesKeepStart(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
