package workspacestore

import (
	"reflect"
	"testing"
)

func seqs(chain []SegmentRow) []int64 {
	out := []int64{}
	for _, segment := range chain {
		out = append(out, segment.Seq)
	}
	return out
}

func TestSelectChain(t *testing.T) {
	compactOnly := []SegmentRow{{Seq: 3, Kind: KindCompact}}
	mixed := []SegmentRow{{Seq: 6, Kind: KindPush}, {Seq: 4, Kind: KindCompact}, {Seq: 5, Kind: KindPush}}
	for _, tc := range []struct {
		name  string
		live  []SegmentRow
		since int64
		want  []int64
	}{
		{"compact only from zero (Phase 1)", compactOnly, 0, []int64{3}},
		{"compact only at its seq (Phase 1)", compactOnly, 3, []int64{3}},
		{"compact only past its seq", compactOnly, 4, []int64{}},
		{"compact and pushes from zero", mixed, 0, []int64{4, 5, 6}},
		{"a reader past the compact gets pushes only", mixed, 5, []int64{6}},
		{"a reader at the head gets nothing", mixed, 6, []int64{}},
		{"a push never stands alone", []SegmentRow{{Seq: 2, Kind: KindPush}}, 0, []int64{}},
		{"a stale push before the compact is skipped", []SegmentRow{{Seq: 1, Kind: KindPush}, {Seq: 2, Kind: KindCompact}}, 0, []int64{2}},
		{"empty", nil, 0, []int64{}},
	} {
		if got := seqs(SelectChain(tc.live, tc.since)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: chain = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidateKind(t *testing.T) {
	a, b := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	for _, tc := range []struct {
		kind  string
		pre   []string
		valid bool
	}{
		{KindCompact, nil, true},
		{KindCompact, []string{a}, false},
		{KindPush, []string{a}, true},
		{KindPush, []string{a, b}, true},
		{KindPush, nil, false},
		{KindPush, []string{a, a}, false},
		{KindPush, []string{"not-a-sha"}, false},
		{KindPush, []string{a, b, a[:39] + "3", a[:39] + "4", a[:39] + "5", a[:39] + "6", a[:39] + "7", a[:39] + "8", a[:39] + "9"}, false},
		{"DELTA", nil, false},
	} {
		if err := validateKind(tc.kind, tc.pre); (err == nil) != tc.valid {
			t.Errorf("validateKind(%s, %v) = %v, want valid %t", tc.kind, tc.pre, err, tc.valid)
		}
	}
}
