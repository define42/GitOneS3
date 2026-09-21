package maintenance

import "testing"

func TestPlanPush(t *testing.T) {
	t.Parallel()

	const mebibyte = int64(1 << 20)
	policy := CompactionPolicy{
		Enabled:           true,
		MaxPackCount:      4,
		MaxSmallPackCount: 2,
		SmallThreshold:    128 * mebibyte,
		MaxInputBytes:     2 << 30,
		MaxInputPacks:     4,
	}

	tests := []struct {
		name           string
		existing       []Pack
		incoming       Pack
		mutatePolicy   func(CompactionPolicy) CompactionPolicy
		wantCompact    []string
		wantRetain     []string
		wantBackground bool
	}{
		{
			name:       "below threshold publishes incoming",
			existing:   []Pack{{ID: "small-1", Size: mebibyte}},
			incoming:   Pack{ID: "incoming", Size: mebibyte},
			wantRetain: []string{"small-1", "incoming"},
		},
		{
			name: "small fragmented subset compacts and large packs stay",
			existing: []Pack{
				{ID: "large-8g", Size: 8 << 30},
				{ID: "small-1", Size: mebibyte},
				{ID: "small-2", Size: 2 * mebibyte},
			},
			incoming:    Pack{ID: "incoming", Size: 3 * mebibyte},
			wantCompact: []string{"small-1", "small-2", "incoming"},
			wantRetain:  []string{"large-8g"},
		},
		{
			name: "oversized live work is deferred",
			existing: []Pack{
				{ID: "small-1", Size: 100 * mebibyte},
				{ID: "small-2", Size: 100 * mebibyte},
				{ID: "small-3", Size: 100 * mebibyte},
			},
			incoming:       Pack{ID: "incoming", Size: 2 << 30},
			wantRetain:     []string{"small-1", "small-2", "small-3", "incoming"},
			wantBackground: true,
		},
		{
			name: "disabled policy never compacts",
			existing: []Pack{
				{ID: "small-1", Size: mebibyte},
				{ID: "small-2", Size: mebibyte},
				{ID: "small-3", Size: mebibyte},
			},
			incoming: Pack{ID: "incoming", Size: mebibyte},
			mutatePolicy: func(input CompactionPolicy) CompactionPolicy {
				input.Enabled = false
				return input
			},
			wantRetain: []string{"small-1", "small-2", "small-3", "incoming"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			casePolicy := policy
			if test.mutatePolicy != nil {
				casePolicy = test.mutatePolicy(casePolicy)
			}
			plan, err := PlanPush(test.existing, test.incoming, casePolicy)
			if err != nil {
				t.Fatalf("PlanPush() error = %v", err)
			}
			assertPackIDs(t, "Compact", plan.Compact, test.wantCompact)
			assertPackIDs(t, "Retain", plan.Retain, test.wantRetain)
			if plan.Background != test.wantBackground {
				t.Fatalf("Background = %t, want %t", plan.Background, test.wantBackground)
			}
		})
	}
}

func assertPackIDs(t *testing.T, field string, packs []Pack, want []string) {
	t.Helper()

	if len(packs) != len(want) {
		t.Fatalf("%s length = %d, want %d", field, len(packs), len(want))
	}
	for index, pack := range packs {
		if pack.ID != want[index] {
			t.Fatalf("%s[%d] = %q, want %q", field, index, pack.ID, want[index])
		}
	}
}
