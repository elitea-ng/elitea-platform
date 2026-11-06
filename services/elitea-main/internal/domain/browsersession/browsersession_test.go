package browsersession

import (
	"encoding/hex"
	"testing"
	"time"
)

// vectorSpec is the spec the supervisor's own test pins
// (browser_session.rs `the_digests_match_mains_vectors`): the two sides
// must hash it to the same bytes, or every view grant is refused.
func vectorSpec() Spec {
	return Spec{
		Revision:    1,
		Name:        NameFor("0123456789abcdef0123456789abcdef"),
		Engine:      1,
		CPUMillis:   1000,
		MemoryBytes: 1 << 30,
		WallSeconds: 600,
		MaxPages:    3,
	}
}

const (
	vectorName        = "elitea-br-86b40f0fcfcf8dcc546fcf51"
	vectorSpecDigest  = "6a3b67da63b9c3426d70c3de912d6697a7537b4bbd5eb2bbe9375ef1bb4f667e"
	vectorWatchDigest = "a8380436929e55a5975f12fd62f73d709ae5ccd2e39acbd5456ac0ea16a9b93f"
	vectorDriveDigest = "a86dce642c3645438769b87f6d7e9d5074d667b9933649151d45437c4a2584ff"
)

func TestDigestsMatchTheSupervisor(t *testing.T) {
	spec := vectorSpec()
	spec2 := SpecDigest(spec)
	watch := ViewDigest(spec, false)
	drive := ViewDigest(spec, true)
	if spec.Name != vectorName {
		t.Errorf("name %s, want %s", spec.Name, vectorName)
	}
	if got := hex.EncodeToString(spec2[:]); got != vectorSpecDigest {
		t.Errorf("spec digest %s, want %s", got, vectorSpecDigest)
	}
	if got := hex.EncodeToString(watch[:]); got != vectorWatchDigest {
		t.Errorf("watch digest %s, want %s", got, vectorWatchDigest)
	}
	if got := hex.EncodeToString(drive[:]); got != vectorDriveDigest {
		t.Errorf("drive digest %s, want %s", got, vectorDriveDigest)
	}
}

func TestNameShape(t *testing.T) {
	if !ValidName(NameFor("exec-1")) {
		t.Fatal("a derived name has the session shape")
	}
	if NameFor("exec-1") == NameFor("exec-2") {
		t.Fatal("two runs share a name")
	}
	for _, bad := range []string{"", "elitea-br-", "elitea-br-0123456789abcdef0123456", "elitea-br-0123456789ABCDEF01234567", "x-elitea-br-0123456789abcdef01234567"} {
		if ValidName(bad) {
			t.Errorf("%q is not a session name", bad)
		}
	}
}

func TestValidateSpec(t *testing.T) {
	if err := ValidateSpec(vectorSpec()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Spec){
		func(s *Spec) { s.Revision = 2 },
		func(s *Spec) { s.Name = "elitea-br-short" },
		func(s *Spec) { s.Engine = 0 },
		func(s *Spec) { s.CPUMillis = 0 },
		func(s *Spec) { s.MemoryBytes = 0 },
		func(s *Spec) { s.WallSeconds = 0 },
		func(s *Spec) { s.MaxPages = 0 },
	} {
		spec := vectorSpec()
		mutate(&spec)
		if ValidateSpec(spec) == nil {
			t.Errorf("%+v is refused", spec)
		}
	}
}

func TestStatesMoveForwardAndWatchability(t *testing.T) {
	if !(StateLive.Rank() < StateLingering.Rank() && StateLingering.Rank() < StateEnded.Rank()) || State("x").Rank() != 0 {
		t.Fatal("state order")
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		session Session
		want    bool
	}{
		{Session{State: StateLive}, true},
		{Session{State: StateLingering, LingerUntil: now.Add(time.Minute)}, true},
		{Session{State: StateLingering, LingerUntil: now.Add(-time.Second)}, false},
		{Session{State: StateEnded}, false},
		// A lingering row with no linger end is not watchable.
		{Session{State: StateLingering}, false},
		// A live row is bounded by its start, its wall and the linger:
		// a worker that never reported the end does not keep it live.
		{Session{State: StateLive, StartedAt: now.Add(-time.Hour), Spec: Spec{WallSeconds: 600}}, false},
		{Session{State: StateLive, StartedAt: now.Add(-15 * time.Minute), Spec: Spec{WallSeconds: 600}}, true},
		{Session{State: StateLive, StartedAt: now.Add(-20 * time.Minute), Spec: Spec{WallSeconds: 600}}, false},
		{Session{State: StateLingering, LingerUntil: now.Add(time.Hour), StartedAt: now.Add(-time.Hour), Spec: Spec{WallSeconds: 600}}, false},
	}
	for _, c := range cases {
		if got := c.session.Watchable(now); got != c.want {
			t.Errorf("%+v watchable=%v, want %v", c.session, got, c.want)
		}
	}
}
