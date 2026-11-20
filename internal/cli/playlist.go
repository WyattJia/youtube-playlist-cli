package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/WyattJia/youtube-playlist-cli/internal/playlist"
	"github.com/WyattJia/youtube-playlist-cli/internal/store"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

func (r *runtime) playlistCommand(a args, s playlist.Service) (result, error) {
	ctx := r.opts.Context
	dry := a.enabled("dry-run")
	remote := a.enabled("remote")

	switch a.command {
	case "sync", "list", "create":
		if len(a.pos) != 0 {
			return result{}, errors.New("此命令不接收位置参数")
		}
	}
	switch a.command {
	case "sync":
		var id string
		if a.has("playlist") {
			var err error
			id, err = videoid.Playlist(a.value("playlist"))
			if err != nil {
				return result{}, err
			}
		}
		p, err := s.Sync(ctx, id, a.enabled("with-items"))
		res := renderList(p)
		if p != nil {
			res.text = fmt.Sprintf("已同步 %d 个歌单。\n", len(p)) + res.text
		}
		return res, err
	case "list":
		p, err := s.List(ctx, remote)
		return renderList(p), err
	case "create":
		edit, err := s.Create(ctx, a.value("title"), a.value("description"), a.value("privacy"), dry)
		return renderEdit(edit, "创建"), err
	case "export":
		return r.export(a, s)
	}

	if len(a.pos) == 0 || (a.command != "add" && len(a.pos) != 1) {
		return result{}, errors.New("需要一个歌单 ID 或链接")
	}
	id, err := videoid.Playlist(a.pos[0])
	if err != nil {
		return result{}, err
	}
	switch a.command {
	case "show":
		entry, err := s.Show(ctx, id, remote)
		if entry.Playlist.ID == "" {
			return result{}, err
		}
		return renderEntry(entry), err
	case "update":
		edit, err := s.Update(ctx, id, youtube.PlaylistPatch{
			Title: a.pointer("title"), Description: a.pointer("description"), Privacy: a.pointer("privacy"),
		}, dry)
		return renderEdit(edit, "更新"), err
	case "delete":
		edit, err := s.Delete(ctx, id, a.enabled("yes"), dry)
		return renderEdit(edit, "删除"), err
	case "add":
		videos, err := parseVideos(a.pos[1:])
		if err != nil {
			return result{}, err
		}
		if a.has("file") {
			fromFile, err := readVideos(a.value("file"))
			if err != nil {
				return result{}, err
			}
			videos = append(videos, fromFile...)
		}
		pos, err := a.intOption("position", 0)
		if err != nil {
			return result{}, err
		}
		ch, err := s.Add(ctx, id, videos, a.enabled("allow-duplicates"), pos, dry)
		return renderChanges(ch), err
	case "remove":
		videos, err := parseVideos(a.flags["video"])
		if err != nil {
			return result{}, err
		}
		ch, err := s.Remove(ctx, id, a.flags["item-id"], videos, a.enabled("all-occurrences"), dry)
		return renderChanges(ch), err
	case "dedupe":
		ch, err := s.Dedupe(ctx, id, a.or("keep", "first"), dry)
		return renderChanges(ch), err
	case "reorder":
		order, err := orderArgs(a)
		if err != nil {
			return result{}, err
		}
		ch, err := s.Reorder(ctx, id, order, dry)
		return renderChanges(ch), err
	default:
		return result{}, errors.New("无法识别歌单命令")
	}
}

func orderArgs(a args) (playlist.Order, error) {
	if a.has("move") == a.has("by") {
		return playlist.Order{}, errors.New("请提供 --move FROM TO 或 --by，二选一")
	}
	if a.has("by") {
		switch by := a.value("by"); by {
		case "title", "channel", "added", "reverse":
			return playlist.Order{By: by}, nil
		default:
			return playlist.Order{}, errors.New("--by 必须是 title、channel、added 或 reverse")
		}
	}
	values := a.flags["move"]
	from, fromErr := strconv.Atoi(values[0])
	to, toErr := strconv.Atoi(values[1])
	if fromErr != nil || toErr != nil || from < 1 || to < 1 {
		return playlist.Order{}, errors.New("--move 需要两个从 1 开始的整数位置")
	}
	return playlist.Order{From: from, To: to}, nil
}

func parseVideos(values []string) ([]string, error) {
	videos := make([]string, 0, len(values))
	for i, value := range values {
		id, err := videoid.Video(value)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个视频参数: %w", i+1, err)
		}
		videos = append(videos, id)
	}
	return videos, nil
}

func readVideos(path string) (videos []string, err error) {
	if path == "" {
		return nil, errors.New("--file 需要文件路径")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开视频列表: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭视频列表: %w", closeErr))
		}
	}()
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		value := strings.TrimSpace(scanner.Text())
		if value == "" {
			continue
		}
		id, err := videoid.Video(value)
		if err != nil {
			return nil, fmt.Errorf("视频文件第 %d 行: %w", line, err)
		}
		videos = append(videos, id)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取视频列表: %w", err)
	}
	return videos, nil
}

func (r *runtime) export(a args, s playlist.Service) (result, error) {
	var id string
	if a.enabled("all") {
		if len(a.pos) != 0 {
			return result{}, errors.New("使用 --all 时不提供歌单 ID")
		}
	} else {
		if len(a.pos) != 1 {
			return result{}, errors.New("需要一个歌单 ID 或 --all")
		}
		var err error
		id, err = videoid.Playlist(a.pos[0])
		if err != nil {
			return result{}, err
		}
	}
	files, err := s.Export(r.opts.Context, id, a.value("out"), a.or("format", "json"), a.enabled("remote"))
	var text strings.Builder
	for _, f := range files {
		fmt.Fprintf(&text, "%s  %s\n", f.PlaylistID, oneLine(f.Path))
		if f.Skipped > 0 {
			fmt.Fprintf(&text, "  %d 个条目缺少有效视频 ID，未写入 TXT。\n", f.Skipped)
		}
	}
	if len(files) == 0 && err == nil {
		text.WriteString("没有待导出的歌单。\n")
	}
	return result{data: files, text: text.String()}, err
}

func renderList(playlists []youtube.Playlist) result {
	if playlists == nil {
		return result{}
	}
	var text strings.Builder
	if len(playlists) == 0 {
		text.WriteString("没有歌单。\n")
	}
	for _, p := range playlists {
		fmt.Fprintf(&text, "%s  %s  %d  %s\n", oneLine(p.ID), oneLine(p.Status.PrivacyStatus),
			p.ContentDetails.ItemCount, oneLine(p.Snippet.Title))
	}
	return result{data: playlists, text: text.String()}
}

func renderEntry(entry store.Entry) result {
	p := entry.Playlist
	var text strings.Builder
	fmt.Fprintf(&text, "%s  %s  %s\n", p.ID, oneLine(p.Status.PrivacyStatus), oneLine(p.Snippet.Title))
	if p.Snippet.Description != "" {
		fmt.Fprintln(&text, oneLine(p.Snippet.Description))
	}
	if !entry.HasItems {
		text.WriteString("本地尚无条目快照。\n")
	} else {
		for _, item := range entry.Items {
			fmt.Fprintf(&text, "%d. %s  %s  %s\n",
				item.Snippet.Position+1, oneLine(item.ID), oneLine(item.VideoID()), oneLine(item.Snippet.Title))
		}
	}
	return result{data: entry, text: text.String()}
}

func renderEdit(edit playlist.EditResult, verb string) result {
	if edit.Playlist.ID == "" && edit.Playlist.Snippet.Title == "" {
		return result{}
	}
	var text strings.Builder
	switch {
	case edit.DryRun:
		fmt.Fprintf(&text, "预演%s", verb)
	case edit.Applied:
		fmt.Fprintf(&text, "已%s", verb)
	case verb == "更新" && len(edit.Parts) == 0:
		text.WriteString("歌单无需修改")
	default:
		// The caller will render the operation error and any structured result.
		return result{data: edit}
	}
	fmt.Fprintf(&text, "  %s  %s", edit.Playlist.ID, oneLine(edit.Playlist.Snippet.Title))
	if privacy := edit.Playlist.Status.PrivacyStatus; privacy != "" {
		fmt.Fprintf(&text, "  %s", oneLine(privacy))
	}
	text.WriteByte('\n')
	if edit.DryRun && edit.Playlist.Snippet.Description != "" {
		fmt.Fprintln(&text, oneLine(edit.Playlist.Snippet.Description))
	}
	if edit.DryRun && len(edit.Parts) > 0 {
		fmt.Fprintf(&text, "修改字段组 %s\n", strings.Join(edit.Parts, ", "))
	}
	return result{data: edit, text: text.String()}
}

func renderChanges(ch playlist.ChangeResult) result {
	var text strings.Builder
	if ch.DryRun {
		fmt.Fprintf(&text, "预演 %d 个操作。\n", len(ch.Changes))
	} else {
		fmt.Fprintf(&text, "已执行 %d/%d 个操作。\n", ch.Applied, len(ch.Changes))
	}
	for _, c := range ch.Changes {
		fmt.Fprint(&text, "  "+strings.TrimSpace(c.ItemID+" "+c.VideoID))
		if c.From != nil {
			fmt.Fprintf(&text, "  %d → %d（从 1 开始）", *c.From+1, *c.Position+1)
		} else if c.Position != nil {
			fmt.Fprintf(&text, "  位置 %d（从 0 开始）", *c.Position)
		}
		text.WriteByte('\n')
	}
	if ch.Skipped > 0 {
		fmt.Fprintf(&text, "跳过 %d 个条目。\n", ch.Skipped)
	}
	if len(ch.NotFoundVideoIDs) > 0 {
		fmt.Fprintln(&text, "歌单中没有这些视频 "+strings.Join(ch.NotFoundVideoIDs, ", "))
	}
	return result{data: ch, text: text.String()}
}
