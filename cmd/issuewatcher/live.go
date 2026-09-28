package main

import (
	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// liveEventNames maps sync events to SSE event names.
var liveEventNames = map[store.EventKind]string{
	store.EventNewIssue:   api.EventItemNew,
	store.EventNewComment: api.EventCommentNew,
	store.EventClosed:     api.EventItemClosed,
}

// liveEvent is the SSE payload for one sync event.
type liveEvent struct {
	ID     int64  `json:"id"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Actor  string `json:"actor,omitempty"`
	Body   string `json:"body,omitempty"`
}

// maxLiveBody bounds a comment preview in a live event (runes).
const maxLiveBody = 280

// publishLive mirrors one sync cycle to the open dashboard tabs: one event per
// change, then sync.status so pages refresh counts.
func publishLive(srv *api.Server, events []store.Event, unread int) {
	if srv == nil {
		return
	}
	for _, e := range events {
		name, ok := liveEventNames[e.Kind]
		if !ok {
			continue
		}
		body := []rune(e.Body)
		if len(body) > maxLiveBody {
			body = append(body[:maxLiveBody-1], '…')
		}
		srv.Publish(name, liveEvent{ID: e.ItemID, Repo: e.Repo, Number: e.Number, Title: e.Title, Actor: e.Actor, Body: string(body)})
	}
	srv.Publish(api.EventSyncStatus, map[string]int{"events": len(events), "unread": unread})
}
