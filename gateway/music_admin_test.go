package main

import (
	"net/http"
	"testing"
)

// Удаление истории прослушиваний — админ-операция; остальные DELETE в music
// (очередь, избранное, плейлисты) — обычные пользовательские действия.
func TestMusicAdminPolicy(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "listens", false},
		{http.MethodGet, "playlists/smart", false},
		{http.MethodDelete, "listens", true},
		{http.MethodDelete, "listens/", true},
		{http.MethodDelete, "queue/1", false},
		{http.MethodDelete, "favorites/a.flac", false},
		{http.MethodDelete, "saved-playlists/mix", false},
		{http.MethodPost, "queue/clear", false},
	}
	for _, tc := range cases {
		if got := adminProtectedPath("music", tc.method, tc.path); got != tc.want {
			t.Errorf("adminProtectedPath(music,%q,%q) = %v, want %v",
				tc.method, tc.path, got, tc.want)
		}
	}
}
