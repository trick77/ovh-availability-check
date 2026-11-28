package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/sse"
)

// The page's live update (user: a full reload every minute made the page jump while reading further down): each
// poll publishes one "poll" event to every open tab, which fetches the page and swaps only the sections that
// changed. No login, a public route: Max bounds the open streams; a tab refused falls back to the full reload.
const (
	maxStreams = 100
	heartbeat  = 30 * time.Second // Traefik and the browser drop a stream silent for too long
)

func newHub() *sse.Hub[int64] { return sse.NewHub[int64](sse.HubOptions{Buffer: 4, Max: maxStreams}) }

// published tells the open tabs a poll ran: the newest only, a slow tab needs no backlog.
func (w *watcher) published() {
	if w.hub != nil {
		w.hub.PublishLatest(time.Now().Unix())
	}
}

func (w *watcher) events(rw http.ResponseWriter, r *http.Request) {
	ch, cancel, ok := w.hub.Subscribe("")
	if !ok { // maxStreams open, or shutting down: the page swaps on a timer instead
		httpx.SetRetryAfter(rw, time.Minute)
		http.Error(rw, "live updates unavailable, the page refreshes every minute", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	s, err := sse.Open(rw)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	defer s.Close()
	s.Heartbeat(heartbeat)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Done():
			return
		case at, open := <-ch:
			if !open {
				return
			}
			if s.Send("poll", strconv.FormatInt(at, 10)) != nil {
				return
			}
		}
	}
}
