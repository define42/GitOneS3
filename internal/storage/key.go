package storage

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

var errInvalidKey = errors.New("invalid object key")

// ValidateKey rejects ambiguous or traversal-like S3 object keys.
func ValidateKey(key string) error {
	return validateKey(key)
}

// ValidatePrefix validates a non-empty object prefix and permits one trailing slash.
func ValidatePrefix(prefix string) error {
	return validateKey(strings.TrimSuffix(prefix, "/"))
}

// ValidateListPage checks the shared bounds and cursor rules for paged listings.
func ValidateListPage(prefix, after string, limit int) error {
	if err := ValidatePrefix(prefix); err != nil {
		return fmt.Errorf("invalid page prefix: %w", err)
	}
	if limit < 1 || limit > MaxListPageSize {
		return fmt.Errorf("page size must be between 1 and %d", MaxListPageSize)
	}
	if after == "" {
		return nil
	}
	if err := ValidateKey(after); err != nil {
		return fmt.Errorf("invalid page cursor: %w", err)
	}
	if !strings.HasPrefix(after, prefix) {
		return errors.New("page cursor is outside the requested prefix")
	}
	return nil
}

func validateKey(key string) error {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) ||
		strings.HasPrefix(key, "/") || strings.Contains(key, `\`) {
		return errInvalidKey
	}
	if path.Clean(key) != key {
		return errInvalidKey
	}
	for index := 0; index < len(key); index++ {
		if key[index] < 0x20 || key[index] == 0x7f {
			return errInvalidKey
		}
	}
	for part := range strings.SplitSeq(key, "/") {
		if part == "" || part == "." || part == ".." {
			return errInvalidKey
		}
	}

	return nil
}
