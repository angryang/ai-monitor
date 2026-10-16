package notifier

import (
	"testing"
	"time"
)

func TestDeduper_FirstFireReturnsTrue(t *testing.T) {
	d := NewDeduper(5 * time.Minute)
	now := time.Unix(123456789, 0).UTC()
	if !d.ShouldFire("t-1:consecutive_failures", now) {
		t.Fatalf("expected true on first fire")
	}
}

func TestDeduper_DedupWithinTTL(t *testing.T) {
	d := NewDeduper(5 * time.Minute)
	now := time.Unix(0, 0).UTC()
	if !d.ShouldFire("k", now) {
		t.Fatalf("first fire must succeed")
	}
	if d.ShouldFire("k", now.Add(1*time.Minute)) {
		t.Fatalf("second fire within TTL must be deduped")
	}
	if d.ShouldFire("k", now.Add(4*time.Minute)) {
		t.Fatalf("fire at TTL-1 must be deduped")
	}
	if !d.ShouldFire("k", now.Add(6*time.Minute)) {
		t.Fatalf("fire after TTL must succeed")
	}
}

func TestDeduper_DifferentKeys(t *testing.T) {
	d := NewDeduper(5 * time.Minute)
	now := time.Unix(0, 0).UTC()
	if !d.ShouldFire("rule_a:task_1", now) {
		t.Fatalf("first fire for key 1 must succeed")
	}
	if !d.ShouldFire("rule_b:task_1", now) {
		t.Fatalf("different rule must be independent")
	}
	if !d.ShouldFire("rule_a:task_2", now) {
		t.Fatalf("different task must be independent")
	}
}

func TestDeduper_Reset(t *testing.T) {
	d := NewDeduper(5 * time.Minute)
	now := time.Unix(0, 0).UTC()
	if !d.ShouldFire("k", now) {
		t.Fatal("first fire fails")
	}
	d.Reset()
	if !d.ShouldFire("k", now.Add(1*time.Second)) {
		t.Fatalf("after reset, k should fire again")
	}
}
