package repository

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git pack checksum, independently checked with SHA-256.
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// verifyMaintenancePack checks the complete stored file, including entries
// omitted by a later manifest and the pack trailer. Memory use is independent
// of pack size. Individual object offsets and CRCs are checked by object().
func (s *Store) verifyMaintenancePack(ctx context.Context, repositoryID, relative, cached string) (err error) {
	if !packKeyPattern.MatchString(relative) {
		return ErrCorrupt
	}
	body, size, err := s.maintenancePackBody(ctx, repositoryID, relative, cached)
	if err != nil {
		return fmt.Errorf("read pack for integrity check: %w", errors.Join(ErrCorrupt, err))
	}
	defer func() { err = errors.Join(err, body.Close()) }()
	if size < 32 || size > MaxPackBytes {
		return ErrCorrupt
	}
	reader := &contextReader{ctx: ctx, reader: body}
	strong := sha256.New()
	git := sha1.New() // #nosec G401 -- Required pack format checksum; SHA-256 is checked separately.
	header := make([]byte, 12)
	if _, err := io.ReadFull(io.TeeReader(reader, io.MultiWriter(strong, git)), header); err != nil {
		return fmt.Errorf("read Git pack header: %w", errors.Join(ErrCorrupt, err))
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if !bytes.Equal(header[:4], []byte("PACK")) || (version != 2 && version != 3) || binary.BigEndian.Uint32(header[8:12]) > MaxGitObjects {
		return ErrCorrupt
	}
	if _, err := io.CopyN(io.MultiWriter(strong, git), reader, size-32); err != nil {
		return fmt.Errorf("hash Git pack contents: %w", errors.Join(ErrCorrupt, err))
	}
	trailer := make([]byte, 20)
	if _, err := io.ReadFull(io.TeeReader(reader, strong), trailer); err != nil {
		return fmt.Errorf("read Git pack trailer: %w", errors.Join(ErrCorrupt, err))
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("git pack length mismatch: %w", errors.Join(ErrCorrupt, err))
	}
	if !bytes.Equal(git.Sum(nil), trailer) || relative != "packs/"+hex.EncodeToString(strong.Sum(nil))+".pack" {
		return fmt.Errorf("git pack checksum mismatch: %w", ErrCorrupt)
	}
	return nil
}

// Reuse the reader's private verified cache when available; otherwise stream
// from storage. Fragmented repositories can exceed the total disk cache limit.
func (s *Store) maintenancePackBody(ctx context.Context, repositoryID, relative, cached string) (io.ReadCloser, int64, error) {
	if cached != "" {
		file, err := os.Open(cached) // #nosec G304 -- Path comes only from GitReader's private temporary directory.
		if err != nil {
			return nil, 0, err
		}
		info, err := file.Stat()
		if err != nil {
			return nil, 0, errors.Join(err, file.Close())
		}
		return file, info.Size(), nil
	}
	body, info, err := s.objects.Get(ctx, "repos/"+repositoryID+"/"+relative)
	return body, info.Size, err
}
