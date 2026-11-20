package reorder

import (
	"slices"
	"testing"
)

func TestPlanExhaustivePermutationsThroughSeven(t *testing.T) {
	for n := 0; n <= 7; n++ {
		current := []string{"a", "b", "c", "d", "e", "f", "g"}[:n]
		desired := slices.Clone(current)
		permutations(desired, 0, func() {
			moves, err := Plan(current, desired)
			if err != nil {
				t.Fatal(err)
			}
			if want := n - lcsLength(current, desired); len(moves) != want {
				t.Fatalf("%v -> %v: got %d moves, want %d", current, desired, len(moves), want)
			}
			order := slices.Clone(current)
			for _, m := range moves {
				if m.From < 0 || m.From >= n || m.To < 0 || m.To >= n ||
					m.From == m.To || order[m.From] != m.ItemID {
					t.Fatalf("%v -> %v: invalid move %+v at %v", current, desired, m, order)
				}
				order = slices.Delete(order, m.From, m.From+1)
				order = slices.Insert(order, m.To, m.ItemID)
			}
			if !slices.Equal(order, desired) {
				t.Fatalf("%v -> %v: moves %+v got %v", current, desired, moves, order)
			}
		})
	}
}

func permutations(items []string, from int, visit func()) {
	if from == len(items) {
		visit()
		return
	}
	for i := from; i < len(items); i++ {
		items[from], items[i] = items[i], items[from]
		permutations(items, from+1, visit)
		items[from], items[i] = items[i], items[from]
	}
}

func lcsLength(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i, x := range a {
		for j, y := range b {
			if x == y {
				dp[i+1][j+1] = dp[i][j] + 1
			} else {
				dp[i+1][j+1] = max(dp[i][j+1], dp[i+1][j])
			}
		}
	}
	return dp[len(a)][len(b)]
}

func TestPlanRejectsInvalidPermutations(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"a"}, {"a", "b"}},
		{{"a"}, {"b"}},
		{{"a", "a"}, {"a", "b"}},
		{{"a", "b"}, {"a", "a"}},
		{{""}, {""}},
	} {
		if _, err := Plan(pair[0], pair[1]); err == nil {
			t.Fatalf("invalid permutations accepted: %v", pair)
		}
	}
}
