package youtube

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
)

const (
	playlistParts = "snippet,status,contentDetails"
	itemParts     = "snippet,contentDetails,status"
	pageSize      = "50"
)

type responsePage[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"nextPageToken"`
}

func pages[T any](ctx context.Context, c *Client, op operation, q url.Values) ([]T, error) {
	items := make([]T, 0)
	tokens := make(map[string]bool)
	for {
		var p responsePage[T]
		if err := c.call(ctx, op, q, nil, &p); err != nil {
			return nil, err
		}
		items = append(items, p.Items...)
		if p.Next == "" {
			return items, nil
		}
		if tokens[p.Next] {
			return nil, &APIError{Kind: APIRejected, Message: "YouTube Data API 返回重复的分页令牌"}
		}
		tokens[p.Next] = true
		q.Set("pageToken", p.Next)
	}
}

func (c *Client) GetMyChannel(ctx context.Context) (Channel, error) {
	op := operation{"channels.list", http.MethodGet, "channels", 1, true}
	query := url.Values{"part": {"snippet,contentDetails,statistics"}, "mine": {"true"}}
	var p responsePage[Channel]
	if err := c.call(ctx, op, query, nil, &p); err != nil {
		return Channel{}, err
	}
	if len(p.Items) == 0 || !videoid.ValidPlaylist(p.Items[0].ID) {
		return Channel{}, channelMissing()
	}
	return p.Items[0], nil
}

func (c *Client) ListMyPlaylists(ctx context.Context) ([]Playlist, error) {
	op := operation{"playlists.list", http.MethodGet, "playlists", 1, true}
	query := url.Values{"part": {playlistParts}, "mine": {"true"}, "maxResults": {pageSize}}
	return pages[Playlist](ctx, c, op, query)
}

func (c *Client) GetPlaylist(ctx context.Context, id string) (Playlist, error) {
	if !videoid.ValidPlaylist(id) {
		return Playlist{}, videoid.ErrPlaylist
	}
	op := operation{"playlists.list", http.MethodGet, "playlists", 1, false}
	var p responsePage[Playlist]
	if err := c.call(ctx, op, url.Values{"part": {playlistParts}, "id": {id}}, nil, &p); err != nil {
		return Playlist{}, err
	}
	for _, playlist := range p.Items {
		if playlist.ID == id {
			return playlist, nil
		}
	}
	return Playlist{}, &APIError{Kind: ItemNotFound, Message: "未找到指定歌单，或此凭据无权读取"}
}

type writeSnippet struct {
	Title           string `json:"title"`
	Description     string `json:"description"`
	DefaultLanguage string `json:"defaultLanguage,omitempty"`
}

type playlistWrite struct {
	ID      string          `json:"id,omitempty"`
	Snippet *writeSnippet   `json:"snippet,omitempty"`
	Status  *PlaylistStatus `json:"status,omitempty"`
}

func (c *Client) CreatePlaylist(ctx context.Context, title, description, privacy string) (Playlist, error) {
	if privacy == "" {
		privacy = "private"
	}
	payload := playlistWrite{
		Snippet: &writeSnippet{Title: title, Description: description},
		Status:  &PlaylistStatus{PrivacyStatus: privacy},
	}
	var p Playlist
	op := operation{"playlists.insert", http.MethodPost, "playlists", 50, true}
	if err := c.call(ctx, op, url.Values{"part": {"snippet,status"}}, payload, &p); err != nil {
		return Playlist{}, err
	}
	if !videoid.ValidPlaylist(p.ID) {
		return Playlist{}, &APIError{Kind: Transient, Ambiguous: true, Message: "YouTube 未返回新歌单 ID，请先查找已创建的歌单"}
	}
	return p, nil
}

func (c *Client) UpdatePlaylist(ctx context.Context, current Playlist, patch PlaylistPatch) (Playlist, error) {
	if !videoid.ValidPlaylist(current.ID) {
		return Playlist{}, videoid.ErrPlaylist
	}
	next, parts := patch.Apply(current)
	if len(parts) == 0 {
		return current, nil
	}
	payload := playlistWrite{ID: current.ID}
	if slices.Contains(parts, "snippet") {
		payload.Snippet = &writeSnippet{
			Title: next.Snippet.Title, Description: next.Snippet.Description, DefaultLanguage: next.Snippet.DefaultLanguage,
		}
	}
	if slices.Contains(parts, "status") {
		payload.Status = &next.Status
	}
	op := operation{"playlists.update", http.MethodPut, "playlists", 50, true}
	if err := c.call(ctx, op, url.Values{"part": {strings.Join(parts, ",")}}, payload, nil); err != nil {
		return Playlist{}, err
	}
	return next, nil
}

func (c *Client) DeletePlaylist(ctx context.Context, id string) error {
	if !videoid.ValidPlaylist(id) {
		return videoid.ErrPlaylist
	}
	op := operation{"playlists.delete", http.MethodDelete, "playlists", 50, true}
	return c.call(ctx, op, url.Values{"id": {id}}, nil, nil)
}

func (c *Client) ListPlaylistItems(ctx context.Context, id string) ([]PlaylistItem, error) {
	if !videoid.ValidPlaylist(id) {
		return nil, videoid.ErrPlaylist
	}
	op := operation{"playlistItems.list", http.MethodGet, "playlistItems", 1, false}
	query := url.Values{"part": {itemParts}, "playlistId": {id}, "maxResults": {pageSize}}
	return pages[PlaylistItem](ctx, c, op, query)
}

type itemWrite struct {
	ID      string `json:"id,omitempty"`
	Snippet struct {
		PlaylistID string     `json:"playlistId"`
		ResourceID ResourceID `json:"resourceId"`
		Position   *int       `json:"position,omitempty"`
	} `json:"snippet"`
}

func newItemWrite(playlistID, videoID string, position *int) itemWrite {
	var w itemWrite
	w.Snippet.PlaylistID = playlistID
	w.Snippet.ResourceID = ResourceID{Kind: "youtube#video", VideoID: videoID}
	w.Snippet.Position = position
	return w
}

func (c *Client) InsertPlaylistItem(ctx context.Context, playlistID, videoID string, position *int) (PlaylistItem, error) {
	if !videoid.ValidPlaylist(playlistID) {
		return PlaylistItem{}, videoid.ErrPlaylist
	}
	if !videoid.ValidVideo(videoID) {
		return PlaylistItem{}, videoid.ErrVideo
	}
	op := operation{"playlistItems.insert", http.MethodPost, "playlistItems", 50, true}
	var item PlaylistItem
	err := c.call(ctx, op, url.Values{"part": {"snippet"}}, newItemWrite(playlistID, videoID, position), &item)
	if err != nil {
		return PlaylistItem{}, err
	}
	if !videoid.ValidItem(item.ID) {
		return PlaylistItem{}, &APIError{Kind: Transient, Ambiguous: true, Message: "YouTube 未返回新条目 ID，请读取歌单再重试"}
	}
	return item, nil
}

func (c *Client) UpdatePlaylistItemPosition(ctx context.Context, playlistID, itemID, videoID string, position int) error {
	if !videoid.ValidPlaylist(playlistID) {
		return videoid.ErrPlaylist
	}
	if !videoid.ValidVideo(videoID) {
		return videoid.ErrVideo
	}
	if !videoid.ValidItem(itemID) {
		return &APIError{Kind: ItemNotFound, Message: "无效的 YouTube 歌单条目 ID"}
	}
	w := newItemWrite(playlistID, videoID, &position)
	w.ID = itemID
	op := operation{"playlistItems.update", http.MethodPut, "playlistItems", 50, true}
	return c.call(ctx, op, url.Values{"part": {"snippet"}}, w, nil)
}

func (c *Client) DeletePlaylistItem(ctx context.Context, itemID string) error {
	if !videoid.ValidItem(itemID) {
		return &APIError{Kind: ItemNotFound, Message: "无效的 YouTube 歌单条目 ID"}
	}
	op := operation{"playlistItems.delete", http.MethodDelete, "playlistItems", 50, true}
	return c.call(ctx, op, url.Values{"id": {itemID}}, nil, nil)
}

func (c *Client) ListVideos(ctx context.Context, ids []string) ([]Video, error) {
	for _, id := range ids {
		if !videoid.ValidVideo(id) {
			return nil, videoid.ErrVideo
		}
	}
	videos := make([]Video, 0, len(ids))
	op := operation{"videos.list", http.MethodGet, "videos", 1, false}
	for chunk := range slices.Chunk(ids, 50) {
		query := url.Values{"part": {"snippet,contentDetails,status"}, "id": {strings.Join(chunk, ",")}}
		var p responsePage[Video]
		if err := c.call(ctx, op, query, nil, &p); err != nil {
			return nil, err
		}
		videos = append(videos, p.Items...)
	}
	return videos, nil
}
