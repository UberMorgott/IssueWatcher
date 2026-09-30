package provider

import (
	"context"
	"fmt"
	"time"
)

// Scheduling describes how a provider wants to be polled. The syncer's
// scheduler is generic: CurseForge, Nexus Mods and Steam plug in by
// implementing Poller (a plain Provider only gets full reconciles).
type Scheduling struct {
	// PushSupported: the platform can signal changes itself (a webhook relay);
	// polling is then a safety net. No provider has it yet.
	PushSupported bool
	// PollMinInterval is the shortest change-check period the platform tolerates.
	PollMinInterval time.Duration
	// RateBudget is the default number of requests per hour the poller may spend.
	RateBudget int
}

// PollState is a project's change-detection memory, persisted by the store:
// validators (ETags) per exact request URL and the high-water marks requests
// are built from. The URL stays the same while nothing changes, which is what
// lets a conditional request answer 304 (free on GitHub's primary quota).
type PollState struct {
	ETags         map[string]string `json:"etags,omitempty"`        // exact URL → ETag
	IssuesSince   time.Time         `json:"issuesSince,omitzero"`   // newest item update seen
	CommentsSince time.Time         `json:"commentsSince,omitzero"` // newest comment update seen
	// FullAt is the last full read of the project (a reconcile or an overflow
	// re-read), set by the syncer: a page fingerprint taken before it is
	// superseded by that read (modkit.PageChanged).
	FullAt time.Time `json:"fullAt,omitzero"`
}

// Changes is the result of one cheap change check.
type Changes struct {
	Numbers     []int // items to re-read (deduplicated, ascending)
	Overflow    bool  // too many changes for targeted fetches: reconcile the project
	Requests    int   // requests spent (304s included)
	NotModified int   // of them answered 304
	// MinInterval is the platform's requested poll interval (GitHub X-Poll-Interval); 0 = none.
	MinInterval time.Duration
}

// SkippedError is a partial FetchChanged result: the items it lists could not
// be loaded (deleted, transferred, not an issue); every other item was. The
// caller keeps the items, logs these and moves on: re-requesting them would
// fail the same way.
type SkippedError struct {
	Items map[int]error // item number → why it was skipped
}

func (e *SkippedError) Error() string {
	return fmt.Sprintf("%d item(s) skipped", len(e.Items))
}

// Poller is the optional tiered-sync side of a provider.
type Poller interface {
	Scheduling() Scheduling
	// DetectChanges does the cheap check for project and updates st in place.
	DetectChanges(ctx context.Context, project Project, st *PollState) (Changes, error)
	// FetchChanged loads the given items with their full comment threads.
	// Items that cannot be loaded one by one come back as a *SkippedError
	// alongside the loaded items.
	FetchChanged(ctx context.Context, project Project, numbers []int) ([]Item, error)
	// FullReconcile re-reads every item updated at or after since.
	FullReconcile(ctx context.Context, project Project, since time.Time) ([]Item, error)
}

// RateStatus is the platform quota as last reported (GET /api/sync).
type RateStatus struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

// RateReporter is implemented by providers that know their quota.
type RateReporter interface {
	RateStatus() (RateStatus, bool)
}
