package videoid

import (
	"errors"
	"strings"
	"testing"
)

func TestVideoID(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	for _, input := range []string{
		id, "  " + id, "https://www.youtube.com/watch?v=" + id + "&t=3",
		"https://music.youtube.com/watch?v=" + id + "&list=PLtest",
		"http://m.youtube.com/watch?v=" + id,
		"https://youtu.be/" + id + "?si=foo",
		"https://www.youtu.be/" + id,
		"https://youtube.com/shorts/" + id, "https://youtube.com/embed/" + id,
	} {
		if got, err := Video(input); got != id || err != nil {
			t.Errorf("Video(%q) = %q, %v", input, got, err)
		}
	}

	for _, input := range []string{
		"../" + id, "tooShort", "ß" + id, "https://youtube.com.evil.invalid/watch?v=" + id,
		"https://youtube.com@evil.invalid/watch?v=" + id, "https://name@youtube.com/watch?v=" + id,
		"ftp://youtube.com/watch?v=" + id, "https://youtube.com:81/watch?v=" + id,
		"https://youtu.be/" + id + "/suffix", "https://youtube.com/shorts/../" + id,
		"https://youtube.com/watch?v=" + id + "&v=" + id, "https://youtube.com/watch?v=..%2F" + id,
		"https://youtube.com/watch?v=" + id + "%ZZ", "https://youtube.com/playlist?v=" + id,
	} {
		if _, err := Video(input); !errors.Is(err, ErrVideo) {
			t.Errorf("accepted invalid video input %q: %v", input, err)
		}
	}
}

func TestPlaylistID(t *testing.T) {
	for input, want := range map[string]string{
		"PL_test-1":         "PL_test-1",
		"LL":                "LL",
		"LM":                "LM",
		"RDAMVMdQw4w9WgXcQ": "RDAMVMdQw4w9WgXcQ",
		"https://www.youtube.com/playlist?list=PLAbC&t=4":     "PLAbC",
		"https://music.youtube.com/playlist?list=OLAK5uy_xyz": "OLAK5uy_xyz",
		"https://youtu.be/dQw4w9WgXcQ?list=PLAbC":             "PLAbC",
		"https://youtube.com/watch?v=dQw4w9WgXcQ&list=PLAbC":  "PLAbC",
	} {
		if got, err := Playlist(input); got != want || err != nil {
			t.Errorf("Playlist(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{
		"../secret", "a/b", "a\\b", ".", "..", "歌单", strings.Repeat("A", 129),
		"https://youtube.com/playlist?list=..%2Fsecret",
		"https://youtube.com/%2e%2e/playlist?list=PLAbC",
		"https://youtube.com/playlist?list=PLAbC&list=PLDifferent",
		"https://example.invalid/?list=PLAbC",
	} {
		if _, err := Playlist(input); !errors.Is(err, ErrPlaylist) {
			t.Errorf("accepted invalid playlist input %q: %v", input, err)
		}
	}
	if ValidItem("../path") || ValidSource("a/b") || !ValidItem("UEx-Ab_c123") {
		t.Fatal("invalid item or source ID policy")
	}
}
