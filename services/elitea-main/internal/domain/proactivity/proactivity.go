// Package proactivity is the per-agent `proactivity` block (ADR-0050
// decision 2, issue 1618): a heartbeat wakes the agent, a cheap triage
// answers no_op, notify or act, and a daily envelope limits the wakes.
//
// # The block
//
// The block is the `proactivity` key of the version's meta. It is part of
// the version and of the release digest, like `memory` and `tool_envelope`:
//
//	proactivity:
//	  mode: heartbeat            # off | heartbeat
//	  interval: 1h               # 15m to 24h, whole minutes
//	  jitter: 10%                # 0% to 50%
//	  quiet_hours: "22:00-07:00 Europe/Berlin"
//	  watch:
//	    - kind: conversation
//	    - kind: schedule_note
//	    - kind: toolkit_read
//	      tool: github/list_pull_requests
//	  act_target: home_thread    # home_thread | new_conversation
//	  limits:
//	    wakes_per_day: 24
//	    acts_per_day: 4
//	    spend_per_day_usd: 2.00
//
// A version without the block, or with `mode: off`, has no heartbeat. The
// heartbeat always reads the block of the agent's current RELEASE, never
// the block of a draft. The administrator's maximums (Ceilings) narrow the
// limits again. Nothing in the block can raise them.
package proactivity

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	// The quiet-hours time zone must load in the distroless image, which
	// has no zoneinfo.
	_ "time/tzdata"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolenvelope"
)

// MetaKey is the version meta key of the block.
const MetaKey = "proactivity"

// Code is the 422 code of an invalid block.
const Code = "proactivity_invalid"

// ErrInvalid is a block that is not valid. The message names the field.
var ErrInvalid = errors.New("invalid proactivity block")

// The modes.
const (
	ModeOff       = "off"
	ModeHeartbeat = "heartbeat"
)

// The act targets. A `workspace_thread:<id>` target is refused in v1: a
// desktop thread becomes proactive through a workspace schedule after its
// handoff (ADR-0050 decision 2f).
const (
	ActTargetHomeThread      = "home_thread"
	ActTargetNewConversation = "new_conversation"
)

// The watch kinds.
const (
	WatchConversation = "conversation"
	WatchScheduleNote = "schedule_note"
	WatchToolkitRead  = "toolkit_read"
)

// The bounds and defaults.
const (
	MinInterval     = 15 * time.Minute
	MaxInterval     = 24 * time.Hour
	DefaultInterval = time.Hour
	DefaultJitter   = 10
	MaxJitter       = 50
	MaxWatch        = 16

	DefaultWakesPerDay    = 24
	DefaultActsPerDay     = 4
	DefaultSpendPerDayUSD = 2.0
	// MaxWakesPerDay is one wake every MinInterval for one day.
	MaxWakesPerDay    = 96
	MaxActsPerDay     = 96
	MaxSpendPerDayUSD = 1000.0
)

// Watch is one source that the triage reads.
type Watch struct {
	Kind string `json:"kind"`
	// Tool is the "<toolkit>/<tool>" key of a toolkit_read source.
	Tool string `json:"tool,omitempty"`
}

// Limits is the daily envelope.
type Limits struct {
	WakesPerDay    int     `json:"wakes_per_day"`
	ActsPerDay     int     `json:"acts_per_day"`
	SpendPerDayUSD float64 `json:"spend_per_day_usd"`
}

// Block is a parsed `proactivity` block.
type Block struct {
	Mode          string
	Interval      time.Duration
	JitterPercent int
	QuietHours    *QuietHours
	Watch         []Watch
	ActTarget     string
	Limits        Limits
}

// Off is the block of a version that has none.
var Off = Block{Mode: ModeOff}

// Enabled reports a heartbeat block.
func (b Block) Enabled() bool { return b.Mode == ModeHeartbeat }

// Watches reports whether the block watches kind.
func (b Block) Watches(kind string) bool {
	return slices.ContainsFunc(b.Watch, func(w Watch) bool { return w.Kind == kind })
}

// ReadTools are the toolkit_read keys, sorted.
func (b Block) ReadTools() []string {
	var tools []string
	for _, w := range b.Watch {
		if w.Kind == WatchToolkitRead {
			tools = append(tools, w.Tool)
		}
	}
	slices.Sort(tools)
	return tools
}

// Check checks meta.proactivity of a version meta. A nil meta and an
// absent key are valid.
func Check(meta map[string]any) error {
	if meta == nil {
		return nil
	}
	_, err := Parse(meta[MetaKey])
	return err
}

// FromMeta reads the block of a stored version meta. A block that is not
// valid is Off: a stored value that the save path did not write must not
// wake an agent.
func FromMeta(meta map[string]any) Block {
	if meta == nil {
		return Off
	}
	block, err := Parse(meta[MetaKey])
	if err != nil {
		return Off
	}
	return block
}

// Parse reads the block. nil is Off. An absent field takes its default.
func Parse(raw any) (Block, error) {
	if raw == nil {
		return Off, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return Block{}, fmt.Errorf("%w: proactivity must be an object", ErrInvalid)
	}
	block := Block{
		Mode: ModeHeartbeat, Interval: DefaultInterval, JitterPercent: DefaultJitter,
		ActTarget: ActTargetHomeThread,
		Watch:     []Watch{{Kind: WatchConversation}, {Kind: WatchScheduleNote}},
		Limits: Limits{
			WakesPerDay: DefaultWakesPerDay, ActsPerDay: DefaultActsPerDay, SpendPerDayUSD: DefaultSpendPerDayUSD,
		},
	}
	for key, value := range object {
		var err error
		switch key {
		case "mode":
			mode, _ := value.(string)
			if mode != ModeOff && mode != ModeHeartbeat {
				err = fmt.Errorf("%w: proactivity.mode must be off or heartbeat", ErrInvalid)
			}
			block.Mode = mode
		case "interval":
			block.Interval, err = parseInterval(value)
		case "jitter":
			block.JitterPercent, err = parseJitter(value)
		case "quiet_hours":
			block.QuietHours, err = parseQuietHoursValue(value)
		case "watch":
			block.Watch, err = parseWatch(value)
		case "act_target":
			block.ActTarget, err = parseActTarget(value)
		case "limits":
			err = parseLimits(value, &block.Limits)
		default:
			err = fmt.Errorf("%w: unknown key proactivity.%s", ErrInvalid, key)
		}
		if err != nil {
			return Block{}, err
		}
	}
	return block, nil
}

func parseInterval(raw any) (time.Duration, error) {
	text, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("%w: proactivity.interval must be a duration such as \"1h\" or \"30m\"", ErrInvalid)
	}
	interval, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil || interval < MinInterval || interval > MaxInterval || interval%time.Minute != 0 {
		return 0, fmt.Errorf("%w: proactivity.interval must be whole minutes from 15m to 24h", ErrInvalid)
	}
	return interval, nil
}

func parseJitter(raw any) (int, error) {
	invalid := fmt.Errorf("%w: proactivity.jitter must be a whole percentage from 0%% to %d%%", ErrInvalid, MaxJitter)
	var number float64
	switch typed := raw.(type) {
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(typed), "%"), 64)
		if err != nil {
			return 0, invalid
		}
		number = parsed
	case float64:
		number = typed
	default:
		return 0, invalid
	}
	if math.IsNaN(number) || number != math.Trunc(number) || number < 0 || number > MaxJitter {
		return 0, invalid
	}
	return int(number), nil
}

func parseQuietHoursValue(raw any) (*QuietHours, error) {
	if raw == nil {
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%w: proactivity.quiet_hours must be a string", ErrInvalid)
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	quiet, err := ParseQuietHours(text)
	if err != nil {
		return nil, err
	}
	return &quiet, nil
}

func parseActTarget(raw any) (string, error) {
	target, _ := raw.(string)
	switch {
	case target == ActTargetHomeThread || target == ActTargetNewConversation:
		return target, nil
	case strings.HasPrefix(target, "workspace_thread:"):
		return "", fmt.Errorf("%w: proactivity.act_target workspace_thread is not available in v1; "+
			"use home_thread or new_conversation", ErrInvalid)
	default:
		return "", fmt.Errorf("%w: proactivity.act_target must be home_thread or new_conversation", ErrInvalid)
	}
}

func parseWatch(raw any) ([]Watch, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: proactivity.watch must be a list", ErrInvalid)
	}
	if len(items) > MaxWatch {
		return nil, fmt.Errorf("%w: proactivity.watch has more than %d entries", ErrInvalid, MaxWatch)
	}
	watch := make([]Watch, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: proactivity.watch entries must be objects", ErrInvalid)
		}
		var entry Watch
		for key, value := range object {
			text, _ := value.(string)
			switch key {
			case "kind":
				entry.Kind = text
			case "tool":
				entry.Tool = text
			default:
				return nil, fmt.Errorf("%w: unknown key proactivity.watch[].%s", ErrInvalid, key)
			}
		}
		switch entry.Kind {
		case WatchConversation, WatchScheduleNote:
			if entry.Tool != "" {
				return nil, fmt.Errorf("%w: proactivity.watch kind %s takes no tool", ErrInvalid, entry.Kind)
			}
		case WatchToolkitRead:
			if !toolenvelope.ValidKey(entry.Tool) || strings.HasSuffix(entry.Tool, "/"+toolenvelope.Wildcard) {
				return nil, fmt.Errorf("%w: proactivity.watch toolkit_read needs a tool \"<toolkit>/<tool>\" with no wildcard", ErrInvalid)
			}
		default:
			return nil, fmt.Errorf("%w: proactivity.watch kind must be conversation, schedule_note or toolkit_read", ErrInvalid)
		}
		if !slices.Contains(watch, entry) {
			watch = append(watch, entry)
		}
	}
	return watch, nil
}

func parseLimits(raw any, limits *Limits) error {
	object, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: proactivity.limits must be an object", ErrInvalid)
	}
	for key, value := range object {
		number, isNumber := value.(float64)
		finite := isNumber && !math.IsNaN(number) && !math.IsInf(number, 0)
		whole := finite && number == math.Trunc(number)
		switch {
		case key == "wakes_per_day" && whole && number >= 1 && number <= MaxWakesPerDay:
			limits.WakesPerDay = int(number)
		case key == "acts_per_day" && whole && number >= 0 && number <= MaxActsPerDay:
			limits.ActsPerDay = int(number)
		case key == "spend_per_day_usd" && finite && number >= 0 && number <= MaxSpendPerDayUSD:
			limits.SpendPerDayUSD = number
		case key == "wakes_per_day":
			return fmt.Errorf("%w: proactivity.limits.wakes_per_day must be a whole number from 1 to %d", ErrInvalid, MaxWakesPerDay)
		case key == "acts_per_day":
			return fmt.Errorf("%w: proactivity.limits.acts_per_day must be a whole number from 0 to %d", ErrInvalid, MaxActsPerDay)
		case key == "spend_per_day_usd":
			return fmt.Errorf("%w: proactivity.limits.spend_per_day_usd must be a number from 0 to %g", ErrInvalid, MaxSpendPerDayUSD)
		default:
			return fmt.Errorf("%w: unknown key proactivity.limits.%s", ErrInvalid, key)
		}
	}
	return nil
}

// QuietHours is a daily window in one time zone. The heartbeat does not
// wake inside it. A window whose start is after its end crosses midnight.
type QuietHours struct {
	// Start and End are minutes after midnight.
	Start, End int
	Location   *time.Location
	text       string
}

// String is the canonical text form.
func (q QuietHours) String() string { return q.text }

// ParseQuietHours reads "HH:MM-HH:MM Zone". The zone is an IANA name. With
// no zone, the window is in UTC.
func ParseQuietHours(text string) (QuietHours, error) {
	invalid := fmt.Errorf("%w: proactivity.quiet_hours must look like \"22:00-07:00 Europe/Berlin\"", ErrInvalid)
	fields := strings.Fields(text)
	if len(fields) == 0 || len(fields) > 2 {
		return QuietHours{}, invalid
	}
	from, to, found := strings.Cut(fields[0], "-")
	if !found {
		return QuietHours{}, invalid
	}
	start, okStart := clockMinutes(from)
	end, okEnd := clockMinutes(to)
	if !okStart || !okEnd || start == end {
		return QuietHours{}, invalid
	}
	location := time.UTC
	if len(fields) == 2 {
		loaded, err := time.LoadLocation(fields[1])
		if err != nil || fields[1] == "Local" {
			return QuietHours{}, fmt.Errorf("%w: proactivity.quiet_hours time zone %q is not known", ErrInvalid, fields[1])
		}
		location = loaded
	}
	return QuietHours{Start: start, End: end, Location: location, text: strings.Join(fields, " ")}, nil
}

func clockMinutes(text string) (int, bool) {
	hours, minutes, found := strings.Cut(text, ":")
	if !found || len(hours) != 2 || len(minutes) != 2 {
		return 0, false
	}
	h, errH := strconv.Atoi(hours)
	m, errM := strconv.Atoi(minutes)
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// Contains reports whether at is inside the window.
func (q QuietHours) Contains(at time.Time) bool {
	local := at.In(q.location())
	minute := local.Hour()*60 + local.Minute()
	if q.Start < q.End {
		return minute >= q.Start && minute < q.End
	}
	return minute >= q.Start || minute < q.End
}

// EndAfter is the first end of the window after at.
func (q QuietHours) EndAfter(at time.Time) time.Time {
	local := at.In(q.location())
	end := time.Date(local.Year(), local.Month(), local.Day(), q.End/60, q.End%60, 0, 0, q.location())
	if !end.After(local) {
		end = time.Date(local.Year(), local.Month(), local.Day()+1, q.End/60, q.End%60, 0, 0, q.location())
	}
	return end
}

func (q QuietHours) location() *time.Location {
	if q.Location == nil {
		return time.UTC
	}
	return q.Location
}

// DayLocation is the time zone of the daily envelope: the quiet-hours
// zone, else UTC.
func (b Block) DayLocation() *time.Location {
	if b.QuietHours != nil {
		return b.QuietHours.location()
	}
	return time.UTC
}

// DayStart is the start of the envelope day that holds at.
func (b Block) DayStart(at time.Time) time.Time {
	local := at.In(b.DayLocation())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, b.DayLocation())
}

// NextDay is the start of the envelope day after the one that holds at.
func (b Block) NextDay(at time.Time) time.Time {
	start := b.DayStart(at)
	return time.Date(start.Year(), start.Month(), start.Day()+1, 0, 0, 0, 0, b.DayLocation())
}

// NextWake is the wake after at: one interval, moved by at most the jitter
// in either direction. unit is a random number in [0, 1).
func (b Block) NextWake(at time.Time, unit float64) time.Time {
	interval := b.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	unit = math.Min(math.Max(unit, 0), 1)
	spread := float64(interval) * float64(b.JitterPercent) / 100
	return at.Add(interval + time.Duration((unit*2-1)*spread))
}

// Ceilings are the administrator's maximums (platformconfig.Proactivity).
type Ceilings struct {
	MaxWakesPerDay    int
	MaxActsPerDay     int
	MaxSpendPerDayUSD float64
	// PersonalActsPerDay caps the acts of an agent in a personal project.
	PersonalActsPerDay int
}

// Effective is the block's limits, narrowed by the ceilings.
func (b Block) Effective(ceilings Ceilings, personal bool) Limits {
	limits := b.Limits
	if ceilings.MaxWakesPerDay > 0 {
		limits.WakesPerDay = min(limits.WakesPerDay, ceilings.MaxWakesPerDay)
	}
	if ceilings.MaxActsPerDay >= 0 {
		limits.ActsPerDay = min(limits.ActsPerDay, ceilings.MaxActsPerDay)
	}
	if personal && ceilings.PersonalActsPerDay >= 0 {
		limits.ActsPerDay = min(limits.ActsPerDay, ceilings.PersonalActsPerDay)
	}
	if ceilings.MaxSpendPerDayUSD >= 0 {
		limits.SpendPerDayUSD = math.Min(limits.SpendPerDayUSD, ceilings.MaxSpendPerDayUSD)
	}
	return limits
}

// Decision is a triage answer.
type Decision string

// The triage answers.
const (
	DecisionNoOp   Decision = "no_op"
	DecisionNotify Decision = "notify"
	DecisionAct    Decision = "act"
)

// Valid reports a known decision.
func (d Decision) Valid() bool {
	return d == DecisionNoOp || d == DecisionNotify || d == DecisionAct
}
