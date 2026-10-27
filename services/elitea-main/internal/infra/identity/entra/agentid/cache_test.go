package agentid

import (
	"testing"
	"time"
)

// The two indexes of the agent cache stay consistent through puts,
// replacements, evictions and the size bound.
func TestAgentCacheIndexes(t *testing.T) {
	c := newAgentCache(2)
	expiry := time.Now().Add(time.Hour)
	c.put("o1", "a1")
	c.putT1("a1", "t1-a1", expiry)
	c.put("o2", "a2")
	if got, ok := c.t1("a1", time.Now()); !ok || got != "t1-a1" {
		t.Fatalf("t1 a1 = %q %v", got, ok)
	}
	if appID, ok := c.appID("o1"); !ok || appID != "a1" {
		t.Fatal("o1 lost")
	}
	// o1 is the most recent now; a third entry evicts o2.
	c.put("o3", "a3")
	if c.knownApp("a2") || c.count() != 2 {
		t.Fatalf("o2 not evicted, count %d", c.count())
	}
	// A new client id for an object drops the old T1 and the old app key.
	c.put("o1", "a1b")
	if c.knownApp("a1") || !c.knownApp("a1b") {
		t.Fatal("app index not moved")
	}
	if _, ok := c.t1("a1b", time.Now()); ok {
		t.Fatal("T1 survived a client id change")
	}
	// An app id seen under another object moves to it.
	c.put("o4", "a3")
	if _, ok := c.appID("o3"); ok {
		t.Fatal("o3 still indexed for a3")
	}
	c.evictApp("a3")
	c.evict("o1")
	if c.count() != 0 || len(c.objects) != 0 || len(c.apps) != 0 {
		t.Fatalf("leftovers: %d %d %d", c.count(), len(c.objects), len(c.apps))
	}
}
