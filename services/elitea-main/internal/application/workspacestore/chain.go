package workspacestore

import "sort"

// SelectChain answers the minimal chain a reader at sinceSeq needs, oldest
// first, from a thread's live (not superseded) segments: the newest COMPACT C
// and every live PUSH after it; or, when sinceSeq is past C's seq (the reader
// already holds a state C leads to), only the live PUSH segments after
// sinceSeq. Nothing when the thread has no live COMPACT: a PUSH never stands
// alone. A COMPACT-only thread answers its one COMPACT for any sinceSeq at or
// below its seq, as Phase 1 did.
//
// A COMPACT supersedes every earlier segment, so the live set is exactly one
// COMPACT and the PUSH segments after it; older rows are skipped defensively.
func SelectChain(live []SegmentRow, sinceSeq int64) []SegmentRow {
	sorted := append([]SegmentRow(nil), live...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	base := -1
	for index, segment := range sorted {
		if segment.Kind == KindCompact {
			base = index
		}
	}
	if base < 0 {
		return []SegmentRow{}
	}
	chain := []SegmentRow{}
	if sinceSeq <= sorted[base].Seq {
		chain = append(chain, sorted[base])
	}
	for _, segment := range sorted[base+1:] {
		if segment.Kind == KindPush && segment.Seq > sinceSeq {
			chain = append(chain, segment)
		}
	}
	return chain
}
