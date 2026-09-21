// Package maintenance contains copy-on-write repository maintenance policy.
package maintenance

import (
	"errors"
	"fmt"
	"math"
)

// Pack describes one immutable canonical pack.
type Pack struct {
	ID   string
	Size int64
}

// CompactionPolicy controls the bounded live-compaction decision on push.
type CompactionPolicy struct {
	Enabled           bool
	MaxPackCount      uint32
	MaxSmallPackCount uint32
	SmallThreshold    int64
	MaxInputBytes     int64
	MaxInputPacks     uint32
}

// CompactionPlan separates immutable packs retained as-is from the subset a
// live worker may rewrite. When Background is true, the incoming pack should
// be published normally and maintenance queued after the push.
type CompactionPlan struct {
	Compact    []Pack
	Retain     []Pack
	Background bool
}

// PlanPush determines whether selected small packs and the incoming pack can
// be compacted within the push latency bounds.
func PlanPush(existing []Pack, incoming Pack, policy CompactionPolicy) (CompactionPlan, error) {
	if err := validatePolicy(policy); err != nil {
		return CompactionPlan{}, err
	}
	if err := validatePacks(existing, incoming); err != nil {
		return CompactionPlan{}, err
	}

	all := make([]Pack, 0, len(existing)+1)
	all = append(all, existing...)
	all = append(all, incoming)
	if !policy.Enabled || !fragmented(all, policy) {
		return CompactionPlan{Retain: all}, nil
	}

	compact := make([]Pack, 0, len(all))
	retain := make([]Pack, 0, len(existing))
	var inputBytes int64
	for _, pack := range existing {
		if pack.Size < policy.SmallThreshold {
			compact = append(compact, pack)
			inputBytes = saturatingAdd(inputBytes, pack.Size)
			continue
		}
		retain = append(retain, pack)
	}
	compact = append(compact, incoming)
	inputBytes = saturatingAdd(inputBytes, incoming.Size)

	if len(compact) < 2 || uint64(len(compact)) > uint64(policy.MaxInputPacks) ||
		inputBytes > policy.MaxInputBytes {
		return CompactionPlan{Retain: all, Background: true}, nil
	}

	return CompactionPlan{Compact: compact, Retain: retain}, nil
}

func validatePolicy(policy CompactionPolicy) error {
	if policy.MaxPackCount == 0 || policy.MaxSmallPackCount == 0 {
		return errors.New("pack-count thresholds must be positive")
	}
	if policy.MaxSmallPackCount > policy.MaxPackCount {
		return errors.New("small-pack threshold cannot exceed total-pack threshold")
	}
	if policy.SmallThreshold <= 0 || policy.MaxInputBytes <= 0 || policy.MaxInputPacks == 0 {
		return errors.New("compaction resource limits must be positive")
	}

	return nil
}

func validatePacks(existing []Pack, incoming Pack) error {
	seen := make(map[string]struct{}, len(existing)+1)
	for _, pack := range existing {
		if pack.ID == "" {
			return errors.New("pack ID is required")
		}
		if pack.Size < 0 {
			return fmt.Errorf("pack %q has a negative size", pack.ID)
		}
		if _, ok := seen[pack.ID]; ok {
			return fmt.Errorf("duplicate pack ID %q", pack.ID)
		}
		seen[pack.ID] = struct{}{}
	}
	if incoming.ID == "" {
		return errors.New("incoming pack ID is required")
	}
	if incoming.Size < 0 {
		return fmt.Errorf("pack %q has a negative size", incoming.ID)
	}
	if _, ok := seen[incoming.ID]; ok {
		return fmt.Errorf("duplicate pack ID %q", incoming.ID)
	}

	return nil
}

func fragmented(packs []Pack, policy CompactionPolicy) bool {
	if uint64(len(packs)) > uint64(policy.MaxPackCount) {
		return true
	}
	var smallCount uint64
	for _, pack := range packs {
		if pack.Size < policy.SmallThreshold {
			smallCount++
		}
	}

	return smallCount > uint64(policy.MaxSmallPackCount)
}

func saturatingAdd(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}

	return left + right
}
