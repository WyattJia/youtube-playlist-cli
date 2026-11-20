package queue

import (
	"slices"

	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

type trackIDs struct {
	requested  string
	equivalent string
	verified   bool
}

func (t trackIDs) alternate(id string) string {
	if id == t.requested {
		return t.equivalent
	}
	return t.requested
}

func (t trackIDs) remoteCount(counts map[string]int) int {
	count := counts[t.requested]
	if t.equivalent != "" {
		count += counts[t.equivalent]
	}
	return count
}

func plannedTrackIDs(p *Playlist) []trackIDs {
	// Pair occurrences by requested ID, in legacy order for repeated IDs.
	verified := make(map[string][]string)
	for i, requested := range p.VerifiedRequestedVideoIDs {
		verified[requested] = append(verified[requested], p.VerifiedActualVideoIDs[i])
	}
	tracks := make([]trackIDs, len(p.Tracks))
	for i, t := range p.Tracks {
		ids := trackIDs{requested: t.VideoID}
		if t.ActualVideoID != "" && t.ActualVideoID != t.VideoID {
			ids.verified = true
			ids.equivalent = t.ActualVideoID
		} else if actuals := verified[t.VideoID]; len(actuals) > 0 {
			ids.verified = true
			if actuals[0] != t.VideoID {
				ids.equivalent = actuals[0]
			}
			verified[t.VideoID] = actuals[1:]
		}
		tracks[i] = ids
	}
	return tracks
}

func remoteVideoCounts(items []youtube.PlaylistItem) map[string]int {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.VideoID()]++
	}
	return counts
}

// matchingTracks matches planned occurrences to remote occurrences. Exact
// matches normally suffice. A legacy equivalent adds a second acceptable ID
// for that track. When the two IDs overlap with other tracks, an augmenting
// path can move an earlier match so no remote occurrence is counted twice.
func matchingTracks(p *Playlist, tracks []trackIDs, counts map[string]int, confirmedInsert int) ([]bool, int) {
	matched := make([]bool, len(tracks))
	assigned := make(map[string][]int)
	hasEquivalents := slices.ContainsFunc(tracks, func(ids trackIDs) bool { return ids.equivalent != "" })
	total := 0
	match := func(i int) {
		ids := tracks[i]
		if ids.requested == "" || matched[i] {
			return
		}
		for _, id := range []string{ids.requested, ids.equivalent} {
			if len(assigned[id]) < counts[id] && id != "" {
				assigned[id] = append(assigned[id], i)
				matched[i] = true
				break
			}
		}
		if !matched[i] && hasEquivalents {
			matched[i] = augment(i, tracks, counts, assigned)
		}
		if matched[i] {
			total++
		}
	}
	// Prefer writes already confirmed by a response or readback, then legacy
	// occurrences, when a new track can also consume the same remote video.
	if confirmedInsert >= 0 {
		match(confirmedInsert)
	}
	for i, t := range p.Tracks {
		if t.Status == TrackDone {
			match(i)
		}
	}
	for _, verified := range []bool{true, false} {
		for i, t := range p.Tracks {
			if t.Status != TrackDone && tracks[i].verified == verified {
				match(i)
			}
		}
	}
	return matched, total
}

func augment(start int, tracks []trackIDs, counts map[string]int, assigned map[string][]int) bool {
	type link struct {
		previous string
		track    int
		slot     int
	}
	var pending []string
	parents := make(map[string]link)
	for _, id := range []string{tracks[start].requested, tracks[start].equivalent} {
		if id != "" && counts[id] > 0 {
			parents[id] = link{track: start}
			pending = append(pending, id)
		}
	}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if len(assigned[id]) < counts[id] {
			slot := len(assigned[id])
			assigned[id] = append(assigned[id], 0)
			for {
				p := parents[id]
				assigned[id][slot] = p.track
				if p.previous == "" {
					return true
				}
				id, slot = p.previous, p.slot
			}
		}
		for slot, track := range assigned[id] {
			next := tracks[track].alternate(id)
			if _, seen := parents[next]; seen || next == "" || counts[next] == 0 {
				continue
			}
			parents[next] = link{previous: id, track: track, slot: slot}
			pending = append(pending, next)
		}
	}
	return false
}
