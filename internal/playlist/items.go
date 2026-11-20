package playlist

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/reorder"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

type Change struct {
	ItemID   string `json:"itemId,omitempty"`
	VideoID  string `json:"videoId,omitempty"`
	From     *int   `json:"from,omitempty"`
	Position *int   `json:"position,omitempty"`
}

type ChangeResult struct {
	DryRun           bool     `json:"dryRun"`
	Changes          []Change `json:"changes"`
	Applied          int      `json:"applied"`
	Skipped          int      `json:"skipped,omitempty"`
	NotFoundVideoIDs []string `json:"notFoundVideoIds,omitempty"`
}

func (s Service) Add(ctx context.Context, id string, videos []string, allowDuplicates bool, position *int, dryRun bool) (ChangeResult, error) {
	result := ChangeResult{DryRun: dryRun, Changes: []Change{}}
	if len(videos) == 0 {
		return result, errors.New("至少提供一个视频 ID、链接或 --file")
	}
	if position != nil && *position < 0 {
		return result, errors.New("--position 不能小于 0")
	}

	existing := make(map[string]bool)
	if !allowDuplicates || position != nil {
		items, err := s.readItems(ctx, id)
		if err != nil {
			return result, err
		}
		if position != nil && *position > len(items) {
			return result, fmt.Errorf("--position 大于当前条目数 %d", len(items))
		}
		for _, item := range items {
			existing[item.VideoID()] = true
		}
	}
	for _, video := range videos {
		if !videoid.ValidVideo(video) {
			return result, videoid.ErrVideo
		}
		if !allowDuplicates && existing[video] {
			result.Skipped++
			continue
		}
		change := Change{VideoID: video}
		if position != nil {
			at := *position + len(result.Changes)
			change.Position = &at
		}
		result.Changes = append(result.Changes, change)
		existing[video] = true
	}
	if dryRun || len(result.Changes) == 0 {
		return result, nil
	}

	if err := s.Cache.InvalidateItems(id); err != nil {
		return result, err
	}
	for i, change := range result.Changes {
		item, err := s.API.InsertPlaylistItem(ctx, id, change.VideoID, change.Position)
		if err != nil {
			return result, fmt.Errorf("添加视频 %s: %w", change.VideoID, err)
		}
		result.Changes[i].ItemID = item.ID
		result.Applied++
	}
	return result, nil
}

func (s Service) Remove(ctx context.Context, id string, itemIDs, videos []string, all, dryRun bool) (ChangeResult, error) {
	result := ChangeResult{DryRun: dryRun, Changes: []Change{}}
	if len(itemIDs) == 0 && len(videos) == 0 {
		return result, errors.New("至少提供一个 --item-id 或 --video")
	}
	for _, itemID := range itemIDs {
		if !videoid.ValidItem(itemID) {
			return result, errors.New("--item-id 不是有效的 YouTube 歌单条目 ID")
		}
	}
	items, err := s.readItems(ctx, id)
	if err != nil {
		return result, err
	}
	byID := make(map[string]youtube.PlaylistItem, len(items))
	byVideo := make(map[string][]youtube.PlaylistItem)
	for _, item := range items {
		byID[item.ID] = item
		byVideo[item.VideoID()] = append(byVideo[item.VideoID()], item)
	}
	seen := make(map[string]bool)
	for _, itemID := range itemIDs {
		item, ok := byID[itemID]
		if !ok {
			return result, &youtube.APIError{Kind: youtube.ItemNotFound, Message: "指定条目不在目标歌单中"}
		}
		if !seen[itemID] {
			result.Changes = append(result.Changes, Change{ItemID: itemID, VideoID: item.VideoID()})
			seen[itemID] = true
		}
	}
	videoSeen := make(map[string]bool)
	for _, video := range videos {
		if videoSeen[video] {
			continue
		}
		videoSeen[video] = true
		matches := byVideo[video]
		if len(matches) == 0 {
			result.NotFoundVideoIDs = append(result.NotFoundVideoIDs, video)
		}
		if !all && len(matches) > 1 {
			matches = matches[:1]
		}
		for _, item := range matches {
			if !seen[item.ID] {
				result.Changes = append(result.Changes, Change{ItemID: item.ID, VideoID: video})
				seen[item.ID] = true
			}
		}
	}
	return s.deleteItems(ctx, id, result)
}

func (s Service) Dedupe(ctx context.Context, id, keep string, dryRun bool) (ChangeResult, error) {
	result := ChangeResult{DryRun: dryRun, Changes: []Change{}}
	if keep != "first" && keep != "last" {
		return result, errors.New("--keep 必须是 first 或 last")
	}
	items, err := s.readItems(ctx, id)
	if err != nil {
		return result, err
	}
	if keep == "last" {
		slices.Reverse(items)
	}
	seen := make(map[string]bool)
	for _, item := range items {
		video := item.VideoID()
		if !videoid.ValidVideo(video) {
			result.Skipped++
			continue
		}
		if seen[video] {
			result.Changes = append(result.Changes, Change{ItemID: item.ID, VideoID: video})
		}
		seen[video] = true
	}
	return s.deleteItems(ctx, id, result)
}

func (s Service) deleteItems(ctx context.Context, id string, result ChangeResult) (ChangeResult, error) {
	for _, change := range result.Changes {
		if !videoid.ValidItem(change.ItemID) {
			return result, errors.New("远端列表包含无效条目 ID")
		}
	}
	if result.DryRun || len(result.Changes) == 0 {
		return result, nil
	}
	if err := s.Cache.InvalidateItems(id); err != nil {
		return result, err
	}
	for _, change := range result.Changes {
		if err := s.API.DeletePlaylistItem(ctx, change.ItemID); err != nil {
			return result, fmt.Errorf("删除歌单条目 %s: %w", change.ItemID, err)
		}
		result.Applied++
	}
	return result, nil
}

type Order struct {
	From int
	To   int
	By   string
}

func (s Service) Reorder(ctx context.Context, id string, order Order, dryRun bool) (ChangeResult, error) {
	result := ChangeResult{DryRun: dryRun, Changes: []Change{}}
	items, err := s.readItems(ctx, id)
	if err != nil {
		return result, err
	}
	moves, err := plannedMoves(items, order)
	if err != nil {
		return result, err
	}
	videoIDs := make(map[string]string, len(items))
	for _, item := range items {
		videoIDs[item.ID] = item.VideoID()
	}
	for _, move := range moves {
		video := videoIDs[move.ItemID]
		if !videoid.ValidItem(move.ItemID) || !videoid.ValidVideo(video) {
			return result, errors.New("要移动的条目缺少有效的条目 ID 或视频 ID")
		}
		result.Changes = append(result.Changes, Change{ItemID: move.ItemID, VideoID: video, From: &move.From, Position: &move.To})
	}
	if dryRun || len(moves) == 0 {
		return result, nil
	}
	if err := s.Cache.InvalidateItems(id); err != nil {
		return result, err
	}
	for _, move := range moves {
		if err := s.API.UpdatePlaylistItemPosition(ctx, id, move.ItemID, videoIDs[move.ItemID], move.To); err != nil {
			return result, fmt.Errorf("移动歌单条目 %s: %w", move.ItemID, err)
		}
		result.Applied++
	}
	return result, nil
}

func plannedMoves(items []youtube.PlaylistItem, order Order) ([]reorder.Move, error) {
	if order.By == "" {
		if order.From < 1 || order.From > len(items) || order.To < 1 || order.To > len(items) {
			return nil, fmt.Errorf("--move 位置必须在 1 至 %d 之间", len(items))
		}
		if order.From == order.To {
			return nil, nil
		}
		return []reorder.Move{{ItemID: items[order.From-1].ID, From: order.From - 1, To: order.To - 1}}, nil
	}

	current := make([]string, len(items))
	desired := make([]string, len(items))
	for i, item := range items {
		current[i] = item.ID
	}
	sorted := slices.Clone(items)
	switch order.By {
	case "reverse":
		slices.Reverse(sorted)
	case "title":
		slices.SortStableFunc(sorted, func(a, b youtube.PlaylistItem) int {
			return strings.Compare(strings.ToLower(a.Snippet.Title), strings.ToLower(b.Snippet.Title))
		})
	case "channel":
		slices.SortStableFunc(sorted, func(a, b youtube.PlaylistItem) int {
			return strings.Compare(strings.ToLower(a.Channel()), strings.ToLower(b.Channel()))
		})
	case "added":
		// RFC3339 timestamps can use different offsets and fractional precision.
		timestamps := make(map[string]time.Time, len(items))
		for _, item := range items {
			stamp, err := time.Parse(time.RFC3339Nano, item.Snippet.PublishedAt)
			if err == nil {
				timestamps[item.ID] = stamp
			}
		}
		slices.SortStableFunc(sorted, func(a, b youtube.PlaylistItem) int {
			return timestamps[a.ID].Compare(timestamps[b.ID])
		})
	default:
		return nil, errors.New("--by 必须是 title、channel、added 或 reverse")
	}
	for i, item := range sorted {
		desired[i] = item.ID
	}
	return reorder.Plan(current, desired)
}
