package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/auth"
	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
)

type ErrorKind string

const (
	QuotaExceeded    ErrorKind = "QUOTA_EXCEEDED"
	AuthExpired      ErrorKind = "AUTH_EXPIRED"
	AuthRequired     ErrorKind = "OAUTH_REQUIRED"
	PlaylistFull     ErrorKind = "PLAYLIST_FULL"
	SeriesConflict   ErrorKind = "SERIES_CONFLICT"
	PermissionDenied ErrorKind = "PERMISSION_DENIED"
	ChannelNotFound  ErrorKind = "CHANNEL_NOT_FOUND"
	PlaylistNotFound ErrorKind = "PLAYLIST_NOT_FOUND"
	ItemNotFound     ErrorKind = "ITEM_NOT_FOUND"
	Transient        ErrorKind = "TRANSIENT"
	APIRejected      ErrorKind = "API_ERROR"
)

type APIError struct {
	Kind         ErrorKind
	StatusCode   int
	Reason       string
	GoogleStatus string
	Message      string
	RetryAfter   time.Duration
	RetryAt      time.Time
	// Ambiguous indicates that a dispatched write may already have taken effect.
	Ambiguous bool
	ResetAt   time.Time
}

func (e *APIError) Error() string {
	msg := string(e.Kind) + ": " + e.Message
	if e.StatusCode != 0 {
		if e.Reason != "" {
			msg += fmt.Sprintf(" (HTTP %d, %s)", e.StatusCode, e.Reason)
		} else {
			msg += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
		}
	}
	if e.Ambiguous {
		msg += "，写入结果待核实"
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf("，服务端要求等待 %s", e.RetryAfter)
	}
	if !e.ResetAt.IsZero() {
		msg += "。太平洋时间重置于 " + e.ResetAt.Format(time.RFC3339)
	}
	return msg
}

func KindOf(err error) ErrorKind {
	var apiErr *APIError
	var quotaErr *quota.LimitError
	switch {
	case errors.As(err, &apiErr):
		return apiErr.Kind
	case errors.As(err, &quotaErr):
		return QuotaExceeded
	case errors.Is(err, auth.ErrExpired):
		return AuthExpired
	case errors.Is(err, auth.ErrNoToken):
		return AuthRequired
	default:
		return ""
	}
}

func (e *APIError) IsPermanentTrackError() bool {
	return e.Kind == SeriesConflict ||
		e.Kind == ItemNotFound && unavailableVideoReason(e.Reason)
}

func channelMissing() *APIError {
	return &APIError{
		Kind:    ChannelNotFound,
		Message: "此 OAuth 登录没有可用的 YouTube 频道。请确认所选 Google 账号下的 YouTube 或品牌账号 (Brand Account)，然后运行 ytpl auth login --authuser <账号序号> 重新授权",
	}
}

func decodeError(status int, retryHeader string, body []byte, now time.Time) *APIError {
	var google struct {
		Error struct {
			Errors  []struct{ Reason string } `json:"errors"`
			Status  string                    `json:"status"`
			Message string                    `json:"message"`
		} `json:"error"`
	}
	var reason string
	if err := json.Unmarshal(body, &google); err == nil && len(google.Error.Errors) > 0 {
		reason = google.Error.Errors[0].Reason
	}

	// Google messages and unknown identifiers are remote text. They can include
	// the full request URL or credentials. Return only locally defined messages
	// and known identifiers, even when Google sends a valid JSON error envelope.
	e := &APIError{
		Kind: APIRejected, StatusCode: status, Message: "YouTube Data API 拒绝了请求",
		RetryAfter: retryDelay(retryHeader, now),
	}
	if e.RetryAfter > 0 {
		e.RetryAt = now.UTC().Add(e.RetryAfter)
	}
	switch google.Error.Status {
	case "INVALID_ARGUMENT", "NOT_FOUND", "PERMISSION_DENIED", "UNAUTHENTICATED",
		"RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "INTERNAL", "UNAVAILABLE", "ABORTED", "DEADLINE_EXCEEDED":
		e.GoogleStatus = google.Error.Status
	}
	switch reason {
	case "quotaExceeded", "dailyLimitExceeded", "playlistContainsMaximumNumberOfVideos",
		"videoAlreadyInAnotherSeriesPlaylist", "channelNotFound", "videoNotFound", "playlistItemNotFound",
		"playlistNotFound", "videoNotAvailable", "invalidVideoId", "rateLimitExceeded", "userRateLimitExceeded",
		"insufficientPermissions", "forbidden", "playlistForbidden", "playlistItemsNotAccessible",
		"playlistOperationUnsupported", "backendError", "authError", "keyInvalid", "accessNotConfigured":
		e.Reason = reason
	}
	switch {
	case status == http.StatusUnauthorized:
		e.Kind, e.Message = AuthExpired, "凭据未获 YouTube Data API 接受，请重新登录或检查 API Key"
	case status == http.StatusForbidden && (reason == "quotaExceeded" || reason == "dailyLimitExceeded"):
		e.Kind, e.Message = QuotaExceeded, "Google 项目的每日 YouTube Data API 配额已用尽"
	case status == http.StatusForbidden && reason == "playlistContainsMaximumNumberOfVideos":
		e.Kind, e.Message = PlaylistFull, "此歌单已达到视频数量上限"
	case status == http.StatusBadRequest && reason == "videoAlreadyInAnotherSeriesPlaylist":
		e.Kind, e.Message = SeriesConflict, "视频已属于另一个系列歌单"
	case status == http.StatusNotFound && reason == "channelNotFound":
		e.Kind, e.Message = ChannelNotFound, channelMissing().Message
	case status == http.StatusNotFound && reason == "playlistNotFound":
		e.Kind, e.Message = PlaylistNotFound, "找不到指定的 YouTube 歌单"
	case status == http.StatusTooManyRequests, status >= 500 && status < 600,
		(status == http.StatusForbidden || status == http.StatusConflict) &&
			(reason == "rateLimitExceeded" || reason == "userRateLimitExceeded"):
		e.Kind, e.Message = Transient, "YouTube Data API 暂时无法处理请求"
	case status >= 400 && status < 500 && unavailableVideoReason(reason):
		e.Kind, e.Message = ItemNotFound, "找不到视频，或视频已不可用"
	case (status == http.StatusNotFound || status == http.StatusBadRequest) && reason == "playlistItemNotFound":
		e.Kind, e.Message = ItemNotFound, "找不到指定的歌单条目"
	case status == http.StatusForbidden:
		e.Kind, e.Message = PermissionDenied, "YouTube Data API 拒绝访问，请检查账号、歌单权限和 Google Cloud 项目设置"
	}
	return e
}

func unavailableVideoReason(reason string) bool {
	switch reason {
	case "videoNotFound", "videoNotAvailable", "invalidVideoId":
		return true
	default:
		return false
	}
}

func retryDelay(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.ParseUint(raw, 10, 64); err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	} else if errors.Is(err, strconv.ErrRange) {
		return time.Duration(math.MaxInt64)
	}
	date, err := http.ParseTime(raw)
	if err != nil {
		date, err = time.Parse(time.RFC1123, raw)
	}
	if err != nil {
		return 0
	}
	return max(0, date.Sub(now))
}
