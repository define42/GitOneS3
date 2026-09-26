package repository

import (
	"strings"
	"testing"
)

func TestParseLFSPointer(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 64)
	valid := string(lfsPointer(oid, 12).Data)
	for _, test := range []struct {
		name    string
		data    string
		pointer bool
		invalid bool
	}{
		{name: "ordinary blob", data: "hello"},
		{name: "pointer", data: valid, pointer: true},
		{name: "CRLF", data: strings.ReplaceAll(valid, "\n", "\r\n"), pointer: true},
		{name: "surrounding whitespace", data: " \n" + valid + "\n\t", pointer: true},
		{name: "legacy version", data: strings.Replace(valid, "https://git-lfs.github.com/spec/v1", "https://hawser.github.com/spec/v1", 1), pointer: true},
		{name: "noncanonical size", data: strings.Replace(valid, "size 12", "size +0012", 1), pointer: true},
		{name: "no final newline", data: strings.TrimSuffix(valid, "\n"), pointer: true},
		{name: "extension", data: valid + "ext-0-example sha256:" + oid + "\n", pointer: true},
		{name: "extension before version", data: "ext-0-example sha256:" + oid + "\n" + valid, pointer: true},
		{name: "wrong digest", data: strings.Replace(valid, "sha256:", "sha1:", 1), pointer: true, invalid: true},
		{name: "negative size", data: strings.Replace(valid, "size 12", "size -1", 1), pointer: true, invalid: true},
		{name: "overflow size", data: strings.Replace(valid, "size 12", "size 9223372036854775808", 1), pointer: true, invalid: true},
		{name: "duplicate size", data: valid + "size 12\n", pointer: true, invalid: true},
		{name: "missing size", data: strings.Replace(valid, "size 12\n", "", 1), pointer: true, invalid: true},
		{name: "too large", data: valid + strings.Repeat("x", 1024)},
	} {
		t.Run(test.name, func(t *testing.T) {
			object, pointer, err := parseLFSPointer([]byte(test.data))
			if pointer != test.pointer || (err != nil) != test.invalid {
				t.Fatalf("parse = %+v, %v, %v", object, pointer, err)
			}
			if pointer && err == nil && (object.OID != oid || object.Size != 12) {
				t.Fatalf("pointer = %+v", object)
			}
		})
	}
}
