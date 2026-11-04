package intervals

import (
	"reflect"
	"testing"
)

// same compares two sets, treating nil and empty as equal.
func same(a, b []Interval) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// covered expands a set into the integers it contains (small ranges only).
func covered(set []Interval) map[int]bool {
	m := map[int]bool{}
	for _, iv := range set {
		for v := iv.Start; v <= iv.End; v++ {
			m[v] = true
		}
	}
	return m
}

func wellFormed(t *testing.T, name string, set []Interval) {
	t.Helper()
	for i, iv := range set {
		if iv.Start > iv.End {
			t.Fatalf("%s: invalid element %v in %v", name, iv, set)
		}
		if i > 0 && set[i-1].End >= iv.Start {
			t.Fatalf("%s: overlapping/touching or unsorted elements in %v", name, set)
		}
	}
}

// fixtures are deterministic pseudo-random interval lists in [-6, 20].
func fixtures() [][]Interval {
	var out [][]Interval
	seed := uint32(12345)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	for k := 0; k < 300; k++ {
		n := next(6)
		var list []Interval
		for i := 0; i < n; i++ {
			a := next(27) - 6
			list = append(list, Interval{a, a + next(5)})
		}
		out = append(out, list)
	}
	return out
}

func TestHiddenMergePreservesCoverage(t *testing.T) {
	for _, in := range fixtures() {
		got, err := Merge(in)
		if err != nil {
			t.Fatal(err)
		}
		wellFormed(t, "Merge", got)
		if !reflect.DeepEqual(covered(got), covered(in)) {
			t.Fatalf("Merge(%v) = %v changes covered integers", in, got)
		}
	}
}

func TestHiddenInsertMatchesMerge(t *testing.T) {
	fx := fixtures()
	for i, in := range fx {
		set, _ := Merge(in)
		x := Interval{i%23 - 5, i%23 - 5 + i%4}
		before := append([]Interval(nil), set...)
		got, err := Insert(set, x)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := Merge(append(append([]Interval(nil), set...), x))
		if !same(got, want) {
			t.Fatalf("Insert(%v, %v) = %v, want %v", set, x, got, want)
		}
		if !same(set, before) {
			t.Fatalf("Insert modified input %v -> %v", before, set)
		}
	}
}

func TestHiddenRemoveIsSetDifference(t *testing.T) {
	fx := fixtures()
	for i, in := range fx {
		set, _ := Merge(in)
		x := Interval{i%19 - 4, i%19 - 4 + i%6}
		before := append([]Interval(nil), set...)
		got, err := Remove(set, x)
		if err != nil {
			t.Fatal(err)
		}
		wellFormedOrSplit := func() {
			for j, iv := range got {
				if iv.Start > iv.End || (j > 0 && got[j-1].End >= iv.Start) {
					t.Fatalf("Remove(%v, %v) = %v is not sorted/valid", set, x, got)
				}
			}
		}
		wellFormedOrSplit()
		want := covered(set)
		for v := x.Start; v <= x.End; v++ {
			delete(want, v)
		}
		if !reflect.DeepEqual(covered(got), want) {
			t.Fatalf("Remove(%v, %v) = %v covers the wrong integers", set, x, got)
		}
		if !same(set, before) {
			t.Fatalf("Remove modified input %v -> %v", before, set)
		}
	}
}

func TestHiddenSinglePointRemoveAndInsert(t *testing.T) {
	got, err := Remove([]Interval{{5, 5}, {7, 9}}, Interval{7, 7})
	if err != nil || !reflect.DeepEqual(got, []Interval{{5, 5}, {8, 9}}) {
		t.Fatalf("Remove point = %v, %v", got, err)
	}
	got, err = Insert([]Interval{{1, 3}, {5, 7}}, Interval{3, 5})
	if err != nil || !reflect.DeepEqual(got, []Interval{{1, 7}}) {
		t.Fatalf("Insert bridging = %v, %v", got, err)
	}
	if _, err := Insert([]Interval{{1, 2}}, Interval{4, 3}); err == nil {
		t.Fatal("Insert invalid should fail")
	}
}
