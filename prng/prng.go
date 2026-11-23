// Package prng implements the versioned SSFD simulation random stream.
package prng

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

const Version = "xoshiro256ss-splitmix64-v1"

// Stream belongs to one logical consumer. Draw order is part of replay.
type Stream struct{ state [4]uint64 }

// New expands a numeric seed, including zero, via four SplitMix64 draws.
func New(seed uint64) *Stream {
	s := new(Stream)
	for i := range s.state {
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		s.state[i] = z ^ z>>31
	}
	return s
}

// Uint64 returns the next reference xoshiro256** word.
func (s *Stream) Uint64() uint64 {
	v := &s.state
	result := bits.RotateLeft64(v[1]*5, 7) * 9
	t := v[1] << 17
	v[2] ^= v[0]
	v[3] ^= v[1]
	v[1] ^= v[2]
	v[0] ^= v[3]
	v[2] ^= t
	v[3] = bits.RotateLeft64(v[3], 45)
	return result
}

// Below draws uniformly from [0, bound) using rejection sampling.
func (s *Stream) Below(bound uint64) (uint64, error) {
	if bound == 0 {
		return 0, errors.New("zero random bound")
	}
	threshold := -bound % bound
	for {
		x := s.Uint64()
		if x >= threshold {
			return x % bound, nil
		}
	}
}

// State encodes the next-draw state as four little-endian words.
func (s *Stream) State() [32]byte {
	var data [32]byte
	for i, word := range s.state {
		binary.LittleEndian.PutUint64(data[i*8:], word)
	}
	return data
}

// Restore loads a checkpoint with the caller-verified stream version.
func Restore(data [32]byte) (*Stream, error) {
	if data == ([32]byte{}) {
		return nil, errors.New("zero xoshiro state")
	}
	s := new(Stream)
	for i := range s.state {
		s.state[i] = binary.LittleEndian.Uint64(data[i*8:])
	}
	return s, nil
}

// RNG is the existing simulation stream under its agent-facing name.
// It has the same state and methods as Stream.
type RNG = Stream
