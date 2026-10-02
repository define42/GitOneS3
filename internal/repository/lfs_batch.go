package repository

import (
	"context"
	"sync"
)

const (
	maxLFSBatchObjects = 1000
	maxLFSBatchReads   = 8
)

// LFSStatResult holds one verified object or its lookup error.
type LFSStatResult struct {
	Object LFSObject
	Err    error
}

// LFSStatBatch checks objects in request order, reading repository identity once
// and checking at most eight objects concurrently. Duplicate identifiers share
// one lookup. Object failures remain independent; cancellation stops all workers
// before returning. Like LFSStat, every lookup verifies the payload's size and
// storage version against its verified record.
func (s *Store) LFSStatBatch(ctx context.Context, namespace, name string, oids []string) ([]LFSStatResult, error) {
	if len(oids) > maxLFSBatchObjects {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := make([]LFSStatResult, len(oids))
	indexes := make(map[string]int, len(oids))
	unique := make([]string, 0, len(oids))
	for i, oid := range oids {
		if !ValidLFSOID(oid) {
			results[i].Err = ErrInvalid
			continue
		}
		if _, exists := indexes[oid]; !exists {
			indexes[oid] = len(unique)
			unique = append(unique, oid)
		}
	}
	if len(unique) == 0 {
		return results, nil
	}
	metadata, metadataErr := s.maintenanceMetadata(ctx, namespace, name)
	lookups := make([]LFSStatResult, len(unique))
	if metadataErr != nil {
		for i := range lookups {
			lookups[i].Err = metadataErr
		}
	} else {
		jobs := make(chan int)
		var workers sync.WaitGroup
		for range min(maxLFSBatchReads, len(unique)) {
			workers.Go(func() {
				for index := range jobs {
					if ctx.Err() != nil {
						return
					}
					record, err := s.checkLFSRecord(ctx, metadata.ID, unique[index])
					lookups[index] = LFSStatResult{Object: record.Object, Err: err}
				}
			})
		}
	queue:
		for i := range unique {
			select {
			case jobs <- i:
			case <-ctx.Done():
				break queue
			}
		}
		close(jobs)
		workers.Wait()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, oid := range oids {
		if results[i].Err == nil {
			results[i] = lookups[indexes[oid]]
		}
	}
	return results, nil
}
