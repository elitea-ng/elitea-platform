package intervals

import (
	"reflect"
	"testing"
)

type iv = Interval

func eq(t *testing.T, name string, got []Interval, err error, want []Interval) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error %v", name, err)
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestMergeOverlapping(t *testing.T) {
	got, err := Merge([]Interval{{8, 10}, {1, 3}, {2, 6}, {15, 18}})
	eq(t, "Merge", got, err, []Interval{{1, 6}, {8, 10}, {15, 18}})
}

func TestMergeTouchingEndpointsCombine(t *testing.T) {
	got, err := Merge([]Interval{{3, 5}, {1, 3}})
	eq(t, "Merge", got, err, []Interval{{1, 5}})
}

func TestMergeAdjacentIntegersStaySeparate(t *testing.T) {
	got, err := Merge([]Interval{{3, 4}, {1, 2}})
	eq(t, "Merge", got, err, []Interval{{1, 2}, {3, 4}})
}

func TestMergeContainedAndPoints(t *testing.T) {
	got, err := Merge([]Interval{{1, 10}, {2, 3}, {4, 4}, {10, 10}, {-5, -5}})
	eq(t, "Merge", got, err, []Interval{{-5, -5}, {1, 10}})
}

func TestMergeDoesNotModifyInput(t *testing.T) {
	in := []Interval{{5, 6}, {1, 2}, {2, 3}}
	snapshot := append([]Interval(nil), in...)
	if _, err := Merge(in); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input modified: %v", in)
	}
}

func TestMergeEmptyAndInvalid(t *testing.T) {
	got, err := Merge(nil)
	eq(t, "Merge(nil)", got, err, nil)
	if _, err := Merge([]Interval{{1, 2}, {5, 4}}); err == nil {
		t.Fatal("Merge with [5,4] should fail")
	}
}

func TestInsertSpanningSeveral(t *testing.T) {
	set := []Interval{{1, 2}, {3, 5}, {6, 7}, {8, 10}, {12, 16}}
	got, err := Insert(set, iv{4, 8})
	eq(t, "Insert", got, err, []Interval{{1, 2}, {3, 10}, {12, 16}})
}

func TestInsertAtEdgesAndTouching(t *testing.T) {
	set := []Interval{{5, 7}}
	got, err := Insert(set, iv{1, 2})
	eq(t, "Insert front", got, err, []Interval{{1, 2}, {5, 7}})
	got, err = Insert(set, iv{7, 9})
	eq(t, "Insert touching", got, err, []Interval{{5, 9}})
	got, err = Insert(set, iv{8, 9})
	eq(t, "Insert adjacent", got, err, []Interval{{5, 7}, {8, 9}})
	got, err = Insert(nil, iv{1, 1})
	eq(t, "Insert into empty", got, err, []Interval{{1, 1}})
	if !reflect.DeepEqual(set, []Interval{{5, 7}}) {
		t.Fatalf("Insert modified its input: %v", set)
	}
}

func TestRemoveSplitsClosedRange(t *testing.T) {
	got, err := Remove([]Interval{{1, 10}}, iv{3, 5})
	eq(t, "Remove", got, err, []Interval{{1, 2}, {6, 10}})
}

func TestRemoveAcrossSeveralAndWhole(t *testing.T) {
	set := []Interval{{1, 4}, {6, 8}, {10, 12}}
	got, err := Remove(set, iv{4, 10})
	eq(t, "Remove", got, err, []Interval{{1, 3}, {11, 12}})
	got, err = Remove(set, iv{0, 100})
	eq(t, "Remove all", got, err, nil)
	if _, err := Remove(set, iv{3, 2}); err == nil {
		t.Fatal("Remove with invalid interval should fail")
	}
}
