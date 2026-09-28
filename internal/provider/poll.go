package provider

import (
	"context"
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

// Poller is the optional tiered-sync side of a provider.
type Poller interface {
	Scheduling() Scheduling
	// DetectChanges does the cheap check for project and updates st in place.
	DetectChanges(ctx context.Context, project Project, st *PollState) (Changes, error)
	// FetchChanged loads the given items with their full comment threads.
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
