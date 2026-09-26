package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"
)

type memoryUpload struct {
	key   string
	parts map[int]memoryPart
}

type memoryPart struct {
	info MultipartPart
	data []byte
}

// CreateMultipart begins an isolated in-memory upload for tests.
func (s *MemoryStore) CreateMultipart(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ValidateKey(key); err != nil {
		return "", fmt.Errorf("create multipart: %w: %w", ErrInvalidMultipart, err)
	}
	id := rand.Text()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploads == nil {
		s.uploads = make(map[string]*memoryUpload)
	}
	s.uploads[id] = &memoryUpload{key: key, parts: make(map[int]memoryPart)}
	return id, nil
}

// UploadPart stores a replayable part, replacing the same numbered part on retry.
func (s *MemoryStore) UploadPart(ctx context.Context, key, uploadID string, number int, body io.ReadSeeker, size int64) (MultipartPart, error) {
	if err := ctx.Err(); err != nil {
		return MultipartPart{}, err
	}
	if err := ValidateMultipart(key, uploadID); err != nil {
		return MultipartPart{}, err
	}
	if err := ValidateMultipartPart(number, size); err != nil {
		return MultipartPart{}, err
	}
	if body == nil {
		return MultipartPart{}, fmt.Errorf("%w: body is required", ErrInvalidMultipart)
	}
	data, err := readExactly(ctx, body, size)
	if err != nil {
		return MultipartPart{}, fmt.Errorf("upload part: %w", err)
	}
	digest := sha256.Sum256(data)
	part := MultipartPart{Number: number, ETag: hex.EncodeToString(digest[:]), Size: size}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return MultipartPart{}, err
	}
	upload, ok := s.uploads[uploadID]
	if !ok || upload.key != key {
		return MultipartPart{}, ErrNotFound
	}
	upload.parts[number] = memoryPart{info: part, data: data}
	return part, nil
}

// CompleteMultipart atomically makes the supplied parts visible as an object.
func (s *MemoryStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if err := ValidateMultipart(key, uploadID); err != nil {
		return ObjectInfo{}, err
	}
	if _, err := MultipartSize(parts); err != nil {
		return ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	upload, ok := s.uploads[uploadID]
	if !ok || upload.key != key {
		return ObjectInfo{}, ErrNotFound
	}
	for _, part := range parts {
		stored, ok := upload.parts[part.Number]
		if !ok || stored.info != part {
			return ObjectInfo{}, fmt.Errorf("%w: part does not match uploaded content", ErrInvalidMultipart)
		}
	}
	var data []byte
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return ObjectInfo{}, err
		}
		data = append(data, upload.parts[part.Number].data...)
	}
	s.next++
	object := memoryObject{data: data, version: Version(strconv.FormatUint(s.next, 10)), lastModified: time.Now().UTC()}
	if s.objects == nil {
		s.objects = make(map[string]memoryObject)
	}
	s.objects[key] = object
	delete(s.uploads, uploadID)
	return objectInfo(key, object), nil
}

// AbortMultipart discards an upload; aborting an absent upload is idempotent.
func (s *MemoryStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateMultipart(key, uploadID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if upload, ok := s.uploads[uploadID]; ok {
		if upload.key != key {
			return ErrNotFound
		}
		delete(s.uploads, uploadID)
	}
	return nil
}

var _ MultipartStore = (*MemoryStore)(nil)
