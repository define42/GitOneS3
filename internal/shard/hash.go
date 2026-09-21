package shard

import (
	"encoding/binary"
	"math/bits"
)

const (
	xxPrime1 uint64 = 11400714785074694791
	xxPrime2 uint64 = 14029467366897019727
	xxPrime3 uint64 = 1609587929392839161
	xxPrime4 uint64 = 9650029242287828579
	xxPrime5 uint64 = 2870177450012600261
)

// Sum64 returns the XXH64 digest of data with seed zero.
//
// The algorithm is implemented here because its output is part of GitOne's
// persistent routing contract. Changing it remaps every top-level namespace.
func Sum64(data []byte) uint64 {
	var hash uint64
	remaining := data

	if len(remaining) >= 32 {
		value1 := xxPrime1
		value1 += xxPrime2
		value2 := xxPrime2
		var value3 uint64
		var value4 uint64
		value4 -= xxPrime1

		for len(remaining) >= 32 {
			value1 = xxRound(value1, binary.LittleEndian.Uint64(remaining[0:8]))
			value2 = xxRound(value2, binary.LittleEndian.Uint64(remaining[8:16]))
			value3 = xxRound(value3, binary.LittleEndian.Uint64(remaining[16:24]))
			value4 = xxRound(value4, binary.LittleEndian.Uint64(remaining[24:32]))
			remaining = remaining[32:]
		}

		hash = bits.RotateLeft64(value1, 1) +
			bits.RotateLeft64(value2, 7) +
			bits.RotateLeft64(value3, 12) +
			bits.RotateLeft64(value4, 18)
		hash = xxMergeRound(hash, value1)
		hash = xxMergeRound(hash, value2)
		hash = xxMergeRound(hash, value3)
		hash = xxMergeRound(hash, value4)
	} else {
		hash = xxPrime5
	}

	hash += uint64(len(data))

	for len(remaining) >= 8 {
		lane := xxRound(0, binary.LittleEndian.Uint64(remaining[:8]))
		hash ^= lane
		hash = bits.RotateLeft64(hash, 27)*xxPrime1 + xxPrime4
		remaining = remaining[8:]
	}

	if len(remaining) >= 4 {
		hash ^= uint64(binary.LittleEndian.Uint32(remaining[:4])) * xxPrime1
		hash = bits.RotateLeft64(hash, 23)*xxPrime2 + xxPrime3
		remaining = remaining[4:]
	}

	for _, value := range remaining {
		hash ^= uint64(value) * xxPrime5
		hash = bits.RotateLeft64(hash, 11) * xxPrime1
	}

	hash ^= hash >> 33
	hash *= xxPrime2
	hash ^= hash >> 29
	hash *= xxPrime3
	hash ^= hash >> 32

	return hash
}

func xxRound(accumulator, input uint64) uint64 {
	accumulator += input * xxPrime2
	accumulator = bits.RotateLeft64(accumulator, 31)
	return accumulator * xxPrime1
}

func xxMergeRound(accumulator, value uint64) uint64 {
	accumulator ^= xxRound(0, value)
	return accumulator*xxPrime1 + xxPrime4
}
