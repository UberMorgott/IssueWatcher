package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Tiered sync (docs/ARCHITECTURE.md → GitHub sync): a cheap conditional REST
// check per repo, targeted GraphQL fetches on change, and the GraphQL issues
// query as the periodic full reconcile.

var _ provider.Poller = (*Provider)(nil)

// pollPage is the REST page size of a change check; a full page means too many
// changes for targeted fetches.
const pollPage = 100

// Scheduling implements provider.Poller.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: time.Minute, RateBudget: 1500}
}

// FullReconcile implements provider.Poller.
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}

// DetectChanges implements provider.Poller: GET issues?state=all&since= and
// issues/comments?since= with If-None-Match. `since` only moves when
// something changed, so an idle repo repeats the exact URL and gets 304.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	var ch provider.Changes
	if _, _, ok := strings.Cut(project.ExternalID, "/"); !ok {
		return ch, fmt.Errorf("github: bad repo id %q", project.ExternalID)
	}
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	base := p.auth.APIURL + "/repos/" + project.ExternalID
	nums := map[int]bool{}

	// Issues (the endpoint also lists pull requests: skipped).
	var issues []struct {
		Number      int       `json:"number"`
		UpdatedAt   time.Time `json:"updated_at"`
		PullRequest *struct{} `json:"pull_request"`
	}
	u := base + "/issues?" + pollQuery(st.IssuesSince, true)
	changed, err := p.poll(ctx, u, st, &ch, &issues)
	if err != nil {
		return ch, err
	}
	if changed {
		seen := st.IssuesSince // the overlap re-lists what was already seen
		for _, is := range issues {
			if !is.UpdatedAt.After(seen) {
				continue
			}
			if is.UpdatedAt.After(st.IssuesSince) {
				st.IssuesSince = is.UpdatedAt
			}
			if is.PullRequest == nil {
				nums[is.Number] = true
			}
		}
		ch.Overflow = ch.Overflow || len(issues) >= pollPage
	}

	// Comments (issues and pull requests share this endpoint; a PR comment's
	// issue_url still says /issues/N, only its html_url says /pull/N).
	var comments []struct {
		IssueURL  string    `json:"issue_url"`
		HTMLURL   string    `json:"html_url"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	u = base + "/issues/comments?" + pollQuery(st.CommentsSince, false)
	changed, err = p.poll(ctx, u, st, &ch, &comments)
	if err != nil {
		return ch, err
	}
	if changed {
		seen := st.CommentsSince
		for _, c := range comments {
			if !c.UpdatedAt.After(seen) {
				continue
			}
			if c.UpdatedAt.After(st.CommentsSince) {
				st.CommentsSince = c.UpdatedAt
			}
			if strings.Contains(c.HTMLURL, "/pull/") {
				continue // pull requests are not synced (the reconcile reads issues only)
			}
			if n, err := strconv.Atoi(c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:]); err == nil {
				nums[n] = true
			}
		}
		ch.Overflow = ch.Overflow || len(comments) >= pollPage
	}
	for n := range nums {
		ch.Numbers = append(ch.Numbers, n)
	}
	slices.Sort(ch.Numbers)
	return ch, nil
}

func pollQuery(since time.Time, issues bool) string {
	q := url.Values{"per_page": {strconv.Itoa(pollPage)}, "sort": {"updated"}, "direction": {"asc"}}
	if issues {
		q.Set("state", "all")
	}
	if !since.IsZero() {
		// One second of overlap: an update in the same second as the last one seen
		// is not missed; re-reading it is cheap and the answer stays 304 when idle.
		q.Set("since", since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	return q.Encode()
}

// poll does one conditional GET; it reports whether the body changed (200).
// Only the current URL of each endpoint keeps its ETag.
func (p *Provider) poll(ctx context.Context, u string, st *provider.PollState, ch *provider.Changes, out any) (bool, error) {
	endpoint := u[:strings.Index(u, "?")]
	etag := st.ETags[u]
	res, err := p.c.conditional(ctx, u, etag)
	ch.Requests++
	if err != nil {
		return false, err
	}
	if res.pollInterval > ch.MinInterval {
		ch.MinInterval = res.pollInterval
	}
	if res.notModified {
		ch.NotModified++
		return false, nil
	}
	for k := range st.ETags { // one validator per endpoint: the exact current URL
		if strings.HasPrefix(k, endpoint+"?") {
			delete(st.ETags, k)
		}
	}
	if res.etag != "" {
		st.ETags[u] = res.etag
	}
	if err := json.Unmarshal(res.body, out); err != nil {
		return false, fmt.Errorf("github: decode %s: %w", endpoint, err)
	}
	return true, nil
}

// changedQuery fetches up to maxBatch issues by number with aliases.
func changedQuery(numbers []int) string {
	var b strings.Builder
	b.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for i, n := range numbers {
		fmt.Fprintf(&b, "    i%d: issue(number: %d) { ...F }\n", i, n)
	}
	b.WriteString("  }\n}\nfragment F on Issue {\n  " + issueFields + "\n}")
	return b.String()
}

const maxBatch = 20

// FetchChanged implements provider.Poller: GraphQL issue(number:) per changed
// number, 20 per request. A number that does not resolve to an issue (a pull
// request, a deleted or transferred issue) fails only its own alias: it is
// reported in a *provider.SkippedError and the rest of the batch is kept.
func (p *Provider) FetchChanged(ctx context.Context, project provider.Project, numbers []int) ([]provider.Item, error) {
	owner, name, ok := strings.Cut(project.ExternalID, "/")
	if !ok {
		return nil, fmt.Errorf("github: bad repo id %q", project.ExternalID)
	}
	var items []provider.Item
	skipped := map[int]error{}
	for start := 0; start < len(numbers); start += maxBatch {
		batch := numbers[start:min(start+maxBatch, len(numbers))]
		var out struct {
			Repository map[string]*gqlIssue `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name}
		errs, err := p.c.graphqlPartial(ctx, changedQuery(batch), vars, &out, minRemaining)
		if err != nil {
			return nil, err
		}
		failed := map[string]string{} // alias → message
		for _, e := range errs {
			alias, ok := aliasOf(e)
			if !ok {
				return nil, errors.New("github graphql: " + e.Message)
			}
			failed[alias] = e.Message
		}
		for i, num := range batch {
			alias := "i" + strconv.Itoa(i)
			if msg, bad := failed[alias]; bad {
				skipped[num] = errors.New("github graphql: " + msg)
				continue
			}
			n := out.Repository[alias]
			if n == nil {
				continue // deleted without an error: nothing to load
			}
			it, err := p.toItem(ctx, n)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
	}
	slices.SortFunc(items, func(a, b provider.Item) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	if len(skipped) > 0 {
		return items, &provider.SkippedError{Items: skipped}
	}
	return items, nil
}

// aliasOf returns the changedQuery alias a GraphQL error is scoped to
// (path ["repository", "iN", ...]).
func aliasOf(e gqlError) (string, bool) {
	if len(e.Path) < 2 || e.Path[0] != "repository" {
		return "", false
	}
	alias, ok := e.Path[1].(string)
	return alias, ok && strings.HasPrefix(alias, "i")
}
