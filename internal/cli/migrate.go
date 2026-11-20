package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/queue"
	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
)

type migrationResult struct {
	queue.RunResult
	quota.Usage
}

func (r *runtime) migrateCommand(a args) (result, error) {
	if len(a.pos) != 0 {
		return result{}, errors.New("migrate 子命令不接收位置参数")
	}
	if a.has("queue-file") && a.value("queue-file") == "" {
		return result{}, errors.New("--queue-file 需要文件路径")
	}
	q := queue.New(r.stateDir, a.value("queue-file"))

	switch a.command {
	case "migrate init":
		included, err := sourceIDs(a.pointer("include"))
		if err != nil {
			return result{}, err
		}
		excluded, err := sourceIDs(a.pointer("exclude"))
		if err != nil {
			return result{}, err
		}
		if a.has("record") && a.value("record") == "" {
			return result{}, errors.New("--record 需要文件路径")
		}
		status, err := q.Init(queue.InitOptions{
			Plan: a.value("plan"), Record: a.value("record"), Include: included, Exclude: excluded, Force: a.enabled("force"),
		})
		if err != nil {
			return result{}, err
		}
		res := renderQueue(status)
		res.text = "已初始化迁移队列。\n" + res.text
		return res, nil

	case "migrate status":
		status, err := q.Status()
		if err != nil {
			return result{}, err
		}
		return renderQueue(status), nil

	case "migrate run":
		budget, err := a.intOption("budget", 0)
		if err != nil {
			return result{}, err
		}
		maxWrites, err := a.intOption("max-writes", 0)
		if err != nil {
			return result{}, err
		}
		id := a.value("playlist")
		if a.has("playlist") && !videoid.ValidSource(id) {
			return result{}, errors.New("--playlist 需要有效的网易云歌单 ID")
		}
		meter := &quota.Meter{Ledger: r.ledger, EnforceReserve: budget == nil, Budget: budget, MaxWrites: maxWrites}
		report, err := q.Run(r.opts.Context, r.api(meter), queue.RunOptions{
			Playlist: id, DryRun: a.enabled("dry-run"), ConfirmRecreate: a.enabled("confirm-recreate"),
			ConfirmReinsert: a.enabled("confirm-reinsert"), Now: r.opts.Now,
		})
		return renderMigration(migrationResult{report, meter.Usage}), err

	case "migrate retry-failed":
		retry, err := q.RetryFailed(a.enabled("include-permanent"))
		if err != nil {
			return result{}, err
		}
		return result{data: retry, text: fmt.Sprintf("已将 %d 个曲目、%d 个歌单设为待重试。\n", retry.Tracks, retry.Playlists)}, nil

	case "migrate export-report":
		path := a.value("out")
		if err := q.ExportReport(path, a.or("format", "json")); err != nil {
			return result{}, err
		}
		return result{data: map[string]string{"path": path}, text: "已写出迁移报告 " + oneLine(path) + "\n"}, nil
	default:
		return result{}, errors.New("无法识别迁移命令")
	}
}

func sourceIDs(value *string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	ids := strings.Split(*value, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
		if !videoid.ValidSource(ids[i]) {
			return nil, errors.New("--include 和 --exclude 需要逗号分隔的网易云歌单 ID")
		}
	}
	return ids, nil
}

func renderQueue(q queue.State) result {
	var text strings.Builder
	fmt.Fprintf(&text, "备份 %s，队列中有 %d 个歌单。\n", q.SourceCommit, len(q.Playlists))
	if q.RetryAfter != nil {
		fmt.Fprintf(&text, "服务端要求暂停请求至 %s。\n", q.RetryAfter.Format(time.RFC3339))
	}
	for _, p := range q.Playlists {
		fmt.Fprintf(&text, "%s  %s  %s  %s\n", p.SourceID, p.Status, p.YouTubeID, oneLine(p.Name))
		if p.Error != "" {
			fmt.Fprintln(&text, "  "+oneLine(p.Error))
		}
		if p.ExtraRemoteTracksCount > 0 {
			fmt.Fprintf(&text, "  保留了 %d 个远端额外条目 (extraRemoteTracksCount)。\n", p.ExtraRemoteTracksCount)
		}
	}
	return result{data: q, text: text.String()}
}

func renderMigration(report migrationResult) result {
	var text strings.Builder
	if report.DryRun {
		fmt.Fprintf(&text, "预演创建 %d 个歌单、插入 %d 个条目。\n", report.PlannedCreates, report.PlannedInserts)
	} else {
		fmt.Fprintf(&text, "创建 %d 个歌单，恢复绑定 %d 个，插入 %d 个条目。\n", report.Created, report.Recovered, report.Inserted)
	}
	fmt.Fprintf(&text, "本次计费 %d 单位，写入尝试 %d 次。\n", report.Units, report.WriteAttempts)
	for _, p := range report.Playlists {
		fmt.Fprintf(&text, "%s  %s  %s\n", p.SourceID, p.Status, p.YouTubeID)
		if p.Error != "" {
			fmt.Fprintln(&text, "  "+oneLine(p.Error))
		}
		if p.ExtraRemoteTracksCount > 0 {
			fmt.Fprintf(&text, "  保留了 %d 个远端额外条目 (extraRemoteTracksCount)。\n", p.ExtraRemoteTracksCount)
		}
	}
	if report.Stopped != nil {
		fmt.Fprintln(&text, "已暂停批次 "+oneLine(report.Stopped.Message))
	}
	return result{data: report, text: text.String()}
}
