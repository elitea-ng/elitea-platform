package repos

// The frame chain behind a recorded view (issue 1893, tenant/0169): an
// answer's entries are stored as a delta against the reader's previous view,
// so a turn costs what changed since that reader's last answer instead of the
// whole conversation. agent_context_view.go writes frames, context_views.go
// reads them back; both reconstruct through readContextViewFrame.
//
// A frame is immutable. A keyframe (no base) holds the full entry list; a
// delta frame holds an edit script against its base's decoded list. The base
// is always an older frame (the table checks base_frame_id < id), so a chain
// ends at a keyframe and never loops; the writer starts a keyframe at depth
// contextViewKeyframeInterval, which bounds a read to that many frames.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// contextViewKeyframeInterval is the chain length at which the writer stores
// a full list again: a read decodes one keyframe and at most this many - 1
// deltas.
const contextViewKeyframeInterval = 64

// contextViewOp is one step of a delta's edit script, applied in base order:
// Keep copies the next Keep base entries, Drop skips the next Drop, Add emits
// its entries. Exactly one is set.
type contextViewOp struct {
	Keep int                                  `json:"keep,omitempty"`
	Drop int                                  `json:"drop,omitempty"`
	Add  []agentexecutionapp.ContextViewEntry `json:"add,omitempty"`
}

// encodeContextViewDelta is an edit script that turns base into next. It is
// one pass with an index of each list's ids: entries both lists hold in the
// same order are kept, entries only base holds are dropped, entries only next
// holds are added, and an entry whose reason changed is dropped and re-added.
// The script is correct for any two lists (applying it to base yields next
// exactly); the indexes only make it short for the usual case, a view that
// grew at the end.
func encodeContextViewDelta(base, next []agentexecutionapp.ContextViewEntry) []contextViewOp {
	inBase := make(map[string]struct{}, len(base))
	for _, entry := range base {
		inBase[entry.MessageID] = struct{}{}
	}
	inNext := make(map[string]struct{}, len(next))
	for _, entry := range next {
		inNext[entry.MessageID] = struct{}{}
	}
	ops := []contextViewOp{}
	keep := func(n int) {
		if last := len(ops) - 1; last >= 0 && ops[last].Keep > 0 {
			ops[last].Keep += n
			return
		}
		ops = append(ops, contextViewOp{Keep: n})
	}
	drop := func(n int) {
		if last := len(ops) - 1; last >= 0 && ops[last].Drop > 0 {
			ops[last].Drop += n
			return
		}
		ops = append(ops, contextViewOp{Drop: n})
	}
	add := func(entry agentexecutionapp.ContextViewEntry) {
		if last := len(ops) - 1; last >= 0 && len(ops[last].Add) > 0 {
			ops[last].Add = append(ops[last].Add, entry)
			return
		}
		ops = append(ops, contextViewOp{Add: []agentexecutionapp.ContextViewEntry{entry}})
	}
	i, j := 0, 0
	for i < len(base) || j < len(next) {
		switch {
		case i < len(base) && j < len(next) && base[i] == next[j]:
			keep(1)
			i++
			j++
		case i < len(base) && j < len(next) && base[i].MessageID == next[j].MessageID:
			// The same message with another reason.
			drop(1)
			add(next[j])
			i++
			j++
		case i < len(base) && !contextViewHas(inNext, base[i].MessageID):
			drop(1)
			i++
		case j < len(next) && !contextViewHas(inBase, next[j].MessageID):
			add(next[j])
			j++
		case i < len(base):
			// Both lists hold both ids, in another order: drop this one
			// here; it is added where next has it.
			drop(1)
			i++
		default:
			add(next[j])
			j++
		}
	}
	return ops
}

func contextViewHas(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// applyContextViewDelta applies an edit script to base. A script that is
// malformed or does not cover base exactly is an error: the chain is damaged
// and the view cannot be told.
func applyContextViewDelta(base []agentexecutionapp.ContextViewEntry, ops []contextViewOp) ([]agentexecutionapp.ContextViewEntry, error) {
	out := make([]agentexecutionapp.ContextViewEntry, 0, len(base))
	i := 0
	for position, op := range ops {
		set := 0
		if op.Keep != 0 {
			set++
		}
		if op.Drop != 0 {
			set++
		}
		if len(op.Add) != 0 {
			set++
		}
		if set != 1 || op.Keep < 0 || op.Drop < 0 {
			return nil, fmt.Errorf("context view delta op %d is malformed", position)
		}
		switch {
		case op.Keep > 0:
			if op.Keep > len(base)-i {
				return nil, fmt.Errorf("context view delta op %d keeps past the base", position)
			}
			out = append(out, base[i:i+op.Keep]...)
			i += op.Keep
		case op.Drop > 0:
			if op.Drop > len(base)-i {
				return nil, fmt.Errorf("context view delta op %d drops past the base", position)
			}
			i += op.Drop
		default:
			out = append(out, op.Add...)
		}
	}
	if i != len(base) {
		return nil, fmt.Errorf("context view delta covers %d of %d base entries", i, len(base))
	}
	return out, nil
}

// contextViewQuerier is the part of a transaction the chain read needs.
type contextViewQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readContextViewFrame decodes the entry list of frame frameID by walking its
// chain to the keyframe. schema qualifies the table ("" for the writer, whose
// search_path is the tenant schema; "p_1." for the reader). A damaged chain
// is an error.
func readContextViewFrame(ctx context.Context, q contextViewQuerier, schema string, frameID int64) ([]agentexecutionapp.ContextViewEntry, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
WITH RECURSIVE chain AS (
    SELECT frame.id, frame.base_frame_id, frame.body, frame.entry_count, 0 AS hop
    FROM %[1]schat_message_context_view_frames frame
    WHERE frame.id = $1
    UNION ALL
    SELECT frame.id, frame.base_frame_id, frame.body, frame.entry_count, chain.hop + 1
    FROM %[1]schat_message_context_view_frames frame
    JOIN chain ON frame.id = chain.base_frame_id
    WHERE chain.hop < $2
)
SELECT id, base_frame_id, body, entry_count FROM chain ORDER BY hop DESC`, schema),
		frameID, contextViewKeyframeInterval*4)
	if err != nil {
		return nil, fmt.Errorf("read context view frames: %w", err)
	}
	defer rows.Close()
	type frame struct {
		id         int64
		base       *int64
		body       []byte
		entryCount int
	}
	var chain []frame
	for rows.Next() {
		var f frame
		if err := rows.Scan(&f.id, &f.base, &f.body, &f.entryCount); err != nil {
			return nil, fmt.Errorf("scan context view frame: %w", err)
		}
		chain = append(chain, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read context view frames: %w", err)
	}
	rows.Close()
	if len(chain) == 0 {
		return nil, fmt.Errorf("context view frame %d is missing", frameID)
	}
	if chain[0].base != nil {
		return nil, fmt.Errorf("context view frame %d has no keyframe within %d frames", frameID, len(chain))
	}
	var entries []agentexecutionapp.ContextViewEntry
	for position, f := range chain {
		if position == 0 {
			if err := json.Unmarshal(f.body, &entries); err != nil {
				return nil, fmt.Errorf("decode context view keyframe %d: %w", f.id, err)
			}
		} else {
			var ops []contextViewOp
			if err := json.Unmarshal(f.body, &ops); err != nil {
				return nil, fmt.Errorf("decode context view frame %d: %w", f.id, err)
			}
			if entries, err = applyContextViewDelta(entries, ops); err != nil {
				return nil, fmt.Errorf("context view frame %d: %w", f.id, err)
			}
		}
		if len(entries) != f.entryCount {
			return nil, fmt.Errorf("context view frame %d decodes to %d entries, recorded %d", f.id, len(entries), f.entryCount)
		}
	}
	if entries == nil {
		entries = []agentexecutionapp.ContextViewEntry{}
	}
	return entries, nil
}
