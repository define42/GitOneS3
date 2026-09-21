package shard

import "testing"

func TestSum64KnownVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		expected uint64
	}{
		{name: "empty", input: []byte{}, expected: 0xef46db3751d8e999},
		{name: "one byte", input: []byte("a"), expected: 0xd24ec4f1a98c6e5b},
		{name: "three bytes", input: []byte("abc"), expected: 0x44bc2cf5ad770999},
		{name: "message digest", input: []byte("message digest"), expected: 0x066ed728fceeb3be},
		{
			name:     "alphabet exercises long lane",
			input:    []byte("abcdefghijklmnopqrstuvwxyz"),
			expected: 0xcfe1f278fa89835c,
		},
		{
			name:     "sixty two bytes exercises long lane",
			input:    []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"),
			expected: 0xaaa46907d3047814,
		},
		{
			name:     "all byte values",
			input:    sequentialBytes(),
			expected: 0x1facbe8406cd904b,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if actual := Sum64(test.input); actual != test.expected {
				t.Fatalf("Sum64() = %#016x, expected %#016x", actual, test.expected)
			}
		})
	}
}

func sequentialBytes() []byte {
	values := make([]byte, 256)
	for index := range values {
		values[index] = byte(index)
	}
	return values
}
