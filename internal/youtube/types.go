// Package youtube implements the YouTube Data API v3 endpoints used by ytpl.
package youtube

type Playlist struct {
	ID             string          `json:"id"`
	Snippet        PlaylistSnippet `json:"snippet"`
	Status         PlaylistStatus  `json:"status"`
	ContentDetails struct {
		ItemCount int `json:"itemCount"`
	} `json:"contentDetails"`
}

type PlaylistSnippet struct {
	Title           string `json:"title"`
	Description     string `json:"description"`
	DefaultLanguage string `json:"defaultLanguage,omitempty"`
	ChannelID       string `json:"channelId,omitempty"`
	ChannelTitle    string `json:"channelTitle,omitempty"`
	PublishedAt     string `json:"publishedAt,omitempty"`
}

type PlaylistStatus struct {
	PrivacyStatus string `json:"privacyStatus"`
	PodcastStatus string `json:"podcastStatus,omitempty"`
}

type PlaylistPatch struct {
	Title       *string
	Description *string
	Privacy     *string
}

// Apply preserves unspecified fields and returns the parts changed by the patch.
func (p PlaylistPatch) Apply(current Playlist) (Playlist, []string) {
	next := current
	if p.Title != nil {
		next.Snippet.Title = *p.Title
	}
	if p.Description != nil {
		next.Snippet.Description = *p.Description
	}
	if p.Privacy != nil {
		next.Status.PrivacyStatus = *p.Privacy
	}
	parts := make([]string, 0, 2)
	if next.Snippet != current.Snippet {
		parts = append(parts, "snippet")
	}
	if next.Status != current.Status {
		parts = append(parts, "status")
	}
	return next, parts
}

type PlaylistItem struct {
	ID      string `json:"id"`
	Snippet struct {
		Title                  string     `json:"title"`
		ChannelTitle           string     `json:"channelTitle,omitempty"`
		VideoOwnerChannelTitle string     `json:"videoOwnerChannelTitle,omitempty"`
		PlaylistID             string     `json:"playlistId,omitempty"`
		Position               int        `json:"position"`
		ResourceID             ResourceID `json:"resourceId"`
		PublishedAt            string     `json:"publishedAt,omitempty"`
	} `json:"snippet"`
	ContentDetails struct {
		VideoID string `json:"videoId,omitempty"`
	} `json:"contentDetails"`
	Status struct {
		PrivacyStatus string `json:"privacyStatus,omitempty"`
	} `json:"status"`
}

func (i PlaylistItem) VideoID() string {
	if i.ContentDetails.VideoID != "" {
		return i.ContentDetails.VideoID
	}
	return i.Snippet.ResourceID.VideoID
}

func (i PlaylistItem) Channel() string {
	if i.Snippet.VideoOwnerChannelTitle != "" {
		return i.Snippet.VideoOwnerChannelTitle
	}
	return i.Snippet.ChannelTitle
}

type ResourceID struct {
	Kind    string `json:"kind"`
	VideoID string `json:"videoId"`
}

type Channel struct {
	ID      string `json:"id"`
	Snippet struct {
		Title     string `json:"title"`
		CustomURL string `json:"customUrl,omitempty"`
	} `json:"snippet"`
	ContentDetails struct {
		RelatedPlaylists struct {
			Uploads string `json:"uploads,omitempty"`
			Likes   string `json:"likes,omitempty"`
		} `json:"relatedPlaylists"`
	} `json:"contentDetails"`
	Statistics struct {
		ViewCount             string `json:"viewCount,omitempty"`
		SubscriberCount       string `json:"subscriberCount,omitempty"`
		HiddenSubscriberCount bool   `json:"hiddenSubscriberCount"`
		VideoCount            string `json:"videoCount,omitempty"`
	} `json:"statistics"`
}

type Video struct {
	ID      string `json:"id"`
	Snippet struct {
		Title        string `json:"title"`
		ChannelID    string `json:"channelId,omitempty"`
		ChannelTitle string `json:"channelTitle,omitempty"`
		PublishedAt  string `json:"publishedAt,omitempty"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration,omitempty"`
	} `json:"contentDetails"`
	Status struct {
		PrivacyStatus string `json:"privacyStatus,omitempty"`
		UploadStatus  string `json:"uploadStatus,omitempty"`
	} `json:"status"`
}
