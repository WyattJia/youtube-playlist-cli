package videoid

import (
	"errors"
	"net/url"
	"strings"
)

var (
	ErrVideo    = errors.New("需要有效的 11 位 YouTube 视频 ID 或视频链接")
	ErrPlaylist = errors.New("需要有效的 YouTube 歌单 ID 或包含 list 参数的链接")
)

func ValidVideo(id string) bool    { return len(id) == 11 && validID(id, 11) }
func ValidPlaylist(id string) bool { return len(id) >= 2 && validID(id, 128) }
func ValidItem(id string) bool     { return validID(id, 256) }
func ValidSource(id string) bool   { return validID(id, 128) }

func validID(id string, maxLength int) bool {
	if len(id) == 0 || len(id) > maxLength {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func Video(input string) (string, error) {
	input = strings.TrimSpace(input)
	if ValidVideo(input) {
		return input, nil
	}
	u, ok := youtubeURL(input)
	if !ok {
		return "", ErrVideo
	}

	var id string
	path := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	switch {
	case shortHost(u.Host) && len(path) == 1:
		id = path[0]
	case !shortHost(u.Host) && u.Path == "/watch":
		id = singleQueryValue(u, "v")
	case !shortHost(u.Host) && len(path) == 2 && (path[0] == "shorts" || path[0] == "embed"):
		id = path[1]
	}
	if !ValidVideo(id) {
		return "", ErrVideo
	}
	return id, nil
}

func Playlist(input string) (string, error) {
	input = strings.TrimSpace(input)
	if ValidPlaylist(input) {
		return input, nil
	}
	u, ok := youtubeURL(input)
	if !ok {
		return "", ErrPlaylist
	}
	id := singleQueryValue(u, "list")
	if !ValidPlaylist(id) {
		return "", ErrPlaylist
	}
	return id, nil
}

func youtubeURL(input string) (*url.URL, bool) {
	u, err := url.Parse(input)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, false
	}
	switch strings.ToLower(u.Host) {
	case "youtube.com", "www.youtube.com", "music.youtube.com", "m.youtube.com", "youtu.be", "www.youtu.be":
	default:
		return nil, false
	}
	for _, component := range strings.Split(u.Path, "/") {
		if component == "." || component == ".." || strings.ContainsRune(component, '\\') {
			return nil, false
		}
	}
	return u, true
}

func shortHost(host string) bool {
	return strings.EqualFold(host, "youtu.be") || strings.EqualFold(host, "www.youtu.be")
}

func singleQueryValue(u *url.URL, name string) string {
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query[name]) != 1 {
		return ""
	}
	return query.Get(name)
}
