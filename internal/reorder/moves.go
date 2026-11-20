// Package reorder plans minimal moves between permutations of unique item IDs.
package reorder

import (
	"errors"
	"slices"
	"sort"
)

type Move struct {
	ItemID string `json:"itemId"`
	From   int    `json:"from"`
	To     int    `json:"to"`
}

// Plan keeps a longest common subsequence of current and desired fixed.
// Every other item moves exactly once. Both indices refer to the order at the
// moment of the move, using the API's zero-based post-removal insertion index.
func Plan(current, desired []string) ([]Move, error) {
	if len(current) != len(desired) {
		return nil, errors.New("排序前后的条目数量不同")
	}
	target := make(map[string]int, len(desired))
	for i, id := range desired {
		if _, duplicate := target[id]; duplicate || id == "" {
			return nil, errors.New("排序目标中存在重复或空条目 ID")
		}
		target[id] = i
	}

	// For unique IDs, LCS is the longest increasing subsequence of the
	// desired positions viewed in current order.
	seen := make([]bool, len(current))
	previous := make([]int, len(current))
	tails := make([]int, 0, len(current))
	for i, id := range current {
		pos, ok := target[id]
		if !ok || seen[pos] {
			return nil, errors.New("排序前后必须包含同一组唯一条目 ID")
		}
		seen[pos] = true
		j := sort.Search(len(tails), func(j int) bool {
			return target[current[tails[j]]] >= pos
		})
		previous[i] = -1
		if j > 0 {
			previous[i] = tails[j-1]
		}
		if j == len(tails) {
			tails = append(tails, i)
		} else {
			tails[j] = i
		}
	}

	moves := make([]Move, 0, len(current)-len(tails))
	if len(tails) == len(current) {
		return moves, nil
	}
	keep := make([]bool, len(desired))
	for i := tails[len(tails)-1]; i >= 0; i = previous[i] {
		keep[target[current[i]]] = true
	}

	order := slices.Clone(current)
	// Anchor each moved item just before the desired suffix. Work right to left
	// so a later move never cuts between this item and its successor.
	for i := len(desired) - 1; i >= 0; i-- {
		if keep[i] {
			continue
		}
		id := desired[i]
		from := slices.Index(order, id)
		to := len(order) - 1
		if i+1 < len(desired) {
			to = slices.Index(order, desired[i+1])
			if from < to {
				to--
			}
		}
		moves = append(moves, Move{ItemID: id, From: from, To: to})
		if from < to {
			copy(order[from:to], order[from+1:to+1])
		} else {
			copy(order[to+1:from+1], order[to:from])
		}
		order[to] = id
	}
	return moves, nil
}
