package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestGoldenFixture(t *testing.T) {
	for range 2 {
		f, err := run(42)
		if err != nil {
			t.Fatal(err)
		}
		var payload bytes.Buffer
		if err := json.NewEncoder(&payload).Encode(f); err != nil {
			t.Fatal(err)
		}
		const want = "0cf83703829d00fa28d8dc373627d10c163eee0b800fcc4db8275bb2fd382fe1"
		if got := fmt.Sprintf("%x", sha256.Sum256(payload.Bytes())); got != want {
			t.Fatalf("seed 42 fixture sha256 = %s, want %s", got, want)
		}
	}
}
