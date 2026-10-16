package egressaudit

import (
	"strings"
	"testing"
	"time"
)

// The merge sums the replicas' LATEST snapshots, per host and per refusal
// reason; the store's contract (storetest) covers when a turn is written.
func TestMergeSumsReplicasPerHostAndReason(t *testing.T) {
	first := time.Unix(1_800_000_000, 0)
	rows := []Row{
		{Report: Report{ReplicaID: "a", ExecutionID: "exec-1", ProjectID: 7, Final: true, Connections: 2, BytesUp: 5, BytesDown: 9,
			HostsReached: []HostReached{{Host: "pypi.org", Port: 443, Connections: 2, BytesUp: 5, BytesDown: 9}}},
			FirstReport: first.Add(time.Second), UpdatedAt: first.Add(time.Minute)},
		{Report: Report{ReplicaID: "b", ExecutionID: "exec-1", ProjectID: 7, ReplicaShutdown: true, Connections: 4, BytesUp: 4, BytesDown: 4, OtherHosts: 1,
			HostsReached: []HostReached{{Host: "pypi.org", Port: 443, Connections: 1, BytesUp: 1, BytesDown: 1}, {Host: "github.com", Port: 443, Connections: 3, BytesUp: 3, BytesDown: 3}},
			HostsRefused: []HostRefused{{Host: "evil.example", Port: 443, Reason: "host-not-allowed", Count: 2}}},
			FirstReport: first, UpdatedAt: first.Add(2 * time.Minute)},
	}
	turn := merge("exec-1", rows)
	if turn.Replicas != 2 || !turn.Complete || turn.ProjectID != 7 || turn.Connections != 6 || turn.OtherHosts != 1 {
		t.Fatalf("merged %+v", turn)
	}
	if !turn.FirstReport.Equal(first) || !turn.LastReport.Equal(first.Add(2*time.Minute)) {
		t.Fatalf("times %v %v", turn.FirstReport, turn.LastReport)
	}
	if len(turn.HostsReached) != 2 || turn.HostsReached[0].Host != "github.com" ||
		turn.HostsReached[1].Connections != 3 || turn.HostsReached[1].BytesDown != 10 {
		t.Fatalf("hosts %+v", turn.HostsReached)
	}
	want := "egress proxy: reached github.com:443, pypi.org:443; refused evil.example:443 (host-not-allowed x2); 1 more"
	if got := Action(turn); got != want {
		t.Fatalf("action %q", got)
	}
	rows[1].ReplicaShutdown = false
	if merge("exec-1", rows).Complete {
		t.Fatal("a turn with an unfinished replica is not complete")
	}
}

func TestMergeRowsGroupsByExecution(t *testing.T) {
	turns := mergeRows([]Row{
		{Report: Report{ReplicaID: "a", ExecutionID: "e2", Final: true}},
		{Report: Report{ReplicaID: "a", ExecutionID: "e1", Final: true}},
		{Report: Report{ReplicaID: "b", ExecutionID: "e2", Final: true}},
	})
	if len(turns) != 2 || turns[0].ExecutionID != "e1" || turns[1].Replicas != 2 {
		t.Fatalf("%+v", turns)
	}
}

func TestActionIsBounded(t *testing.T) {
	turn := Turn{Complete: true}
	for range 200 {
		turn.HostsReached = append(turn.HostsReached, HostReached{Host: "a-long-host-name.example.com", Port: 443})
	}
	if got := Action(turn); len(got) > maxAction || !strings.HasSuffix(got, "...") {
		t.Fatalf("%d %q", len(got), got[len(got)-10:])
	}
}
