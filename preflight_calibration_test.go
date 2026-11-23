package main

import "testing"

func TestPreflightCalibrationRingCount(t *testing.T) {
	c := newPreflightCalibration()
	for i := 0; i < len(c.items); i++ {
		c.add("a", "b")
	}
	if got := c.counts["a|b"]; got != len(c.items) {
		t.Fatalf("full ring count: got %d want %d", got, len(c.items))
	}

	for i := 0; i < 8; i++ {
		c.add("x", "y")
	}
	live := 0
	for _, pair := range c.items {
		if pair == "a|b" {
			live++
		}
	}
	if got := c.counts["a|b"]; got != live {
		t.Fatalf("after eviction: count %d, ring holds %d", got, live)
	}
	if live != len(c.items)-8 {
		t.Fatalf("ring holds %d a|b entries, want %d", live, len(c.items)-8)
	}

	c2 := newPreflightCalibration()
	for i := 0; i < len(c2.items)-1; i++ {
		c2.add("base", "pair")
	}
	c2.add("solo", "pair")
	if _, ok := c2.counts["solo|pair"]; !ok {
		t.Fatal("solo pair missing before its slot was evicted")
	}
	for i := 0; i < len(c2.items); i++ {
		c2.add("flush", "pair")
	}
	if _, ok := c2.counts["solo|pair"]; ok {
		t.Fatal("solo pair still counted after its slot was evicted")
	}
}
