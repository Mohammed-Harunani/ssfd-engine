package prng

import (
	"encoding/binary"
	"testing"
)

func TestReferenceVector(t *testing.T) {
	var state [32]byte
	for i := range 4 {
		binary.LittleEndian.PutUint64(state[i*8:], uint64(i)+1)
	}
	s, err := Restore(state)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint64{11520, 0, 1509978240, 1215971899390074240, 1216172134540287360, 607988272756665600} {
		if got := s.Uint64(); got != want {
			t.Fatalf("word %d: got %d want %d", i, got, want)
		}
	}
}

func TestSeedCheckpointAndBound(t *testing.T) {
	s := New(42)
	if want := uint64(1546998764402558742); s.Uint64() != want {
		t.Fatal("seed expansion changed")
	}
	restored, err := Restore(s.State())
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		a, err := s.Below(1<<63 + 1)
		b, other := restored.Below(1<<63 + 1)
		if err != nil || other != nil || a != b || a >= 1<<63+1 {
			t.Fatal("divergent checkpoint or out-of-range draw")
		}
	}
	if _, err := s.Below(0); err == nil {
		t.Fatal("accepted zero bound")
	}
	if _, err := Restore([32]byte{}); err == nil {
		t.Fatal("accepted absorbing state")
	}
}
