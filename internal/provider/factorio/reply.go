package factorio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// ErrUnknownOutcome: the post may or may not have landed and a read-back did
// not find it.
var ErrUnknownOutcome = modkit.ErrUnknownOutcome

// readBackTimeout bounds the read-back; it runs even when the request that
// posted was cancelled.
const readBackTimeout = 2 * time.Minute

// modOf finds the mod of a thread: remembered from the reads of this run,
// else looked up in the account's discussion lists.
func (p *Provider) modOf(ctx context.Context, id string) (string, error) {
	if m, ok := p.mods.Load(id); ok {
		s, _ := m.(string)
		return s, nil
	}
	projects, err := p.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, pr := range projects {
		for page, last := 1, 1; page <= last && page <= maxPages; page++ {
			rows, lp, err := p.listPage(ctx, pr.ExternalID, page)
			if err != nil {
				return "", err
			}
			last = lp
			for _, r := range rows {
				if r.ID == id {
					return pr.ExternalID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("factorio: thread %s not found on the account's mods", id)
}

// Reply implements provider.Provider: the thread's own reply form, posted
// once; a login page answer is a refusal, anything unclear is read back
// (author + text + time, messages present before ignored), never re-sent.
// Capabilities.Reply stays off until the form is verified live.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	id, ok := strings.CutPrefix(itemExternalID, "thread:")
	if !ok || !threadHrefRe.MatchString("/discussion/"+id) {
		return provider.Comment{}, fmt.Errorf("factorio: bad item id %q", itemExternalID)
	}
	jar := p.jar()
	if jar == nil || jar.Empty() {
		return provider.Comment{}, fmt.Errorf("%w: factorio: not signed in (Settings › Платформы › Подключить)", provider.ErrNotSignedIn)
	}
	account := jar.Account()
	if account == "" {
		a, err := p.whoAmI(ctx, jar)
		if err != nil {
			return provider.Comment{}, err
		}
		account = a
	}
	mod, err := p.modOf(ctx, id)
	if err != nil {
		return provider.Comment{}, err
	}
	lock, _ := p.threads.LoadOrStore(itemExternalID, &sync.Mutex{})
	mu, _ := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	path := "/mod/" + url.PathEscape(mod) + "/discussion/" + id
	page, err := p.get(ctx, path, jar)
	if err != nil {
		return provider.Comment{}, fmt.Errorf("factorio: read thread before reply: %w", err)
	}
	if signedInUser(page) == "" {
		return provider.Comment{}, fmt.Errorf("%w: factorio: the stored session is signed out", provider.ErrRelogin)
	}
	msgs, form, err := parseThread(page)
	if err != nil {
		return provider.Comment{}, err
	}
	if form.Disabled {
		return provider.Comment{}, errors.New("factorio: the reply form is disabled for this account (it must own Factorio)")
	}
	before := map[string]bool{}
	for _, m := range msgs {
		before[m.Author+"|"+m.At] = true
	}
	sent := p.opts.Now()
	status, answer, err := p.submit(ctx, path, form, body, jar)
	switch {
	case err != nil && errors.Is(err, websession.ErrHost): // redirected to the login page
		return provider.Comment{}, fmt.Errorf("%w: factorio: %w", provider.ErrRelogin, err)
	case err == nil && status == http.StatusOK && signedInUser(answer) == "" && strings.Contains(answer, `/login`):
		return provider.Comment{}, fmt.Errorf("%w: factorio: the portal answered its login page", provider.ErrRelogin)
	case err == nil && status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests:
		return provider.Comment{}, fmt.Errorf("factorio: reply refused: HTTP %d", status)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	match := modkit.Match{Account: account, Body: body, Before: before, Sent: sent}
	var ferr error
	for _, wait := range append([]time.Duration{0}, p.opts.ReadBackWaits...) {
		if !modkit.Sleep(rctx, wait) {
			break
		}
		var c provider.Comment
		var found bool
		if c, found, ferr = p.findReply(rctx, mod, id, path, jar, match); ferr == nil && found {
			return c, nil
		}
	}
	p.opts.Log.Warn("factorio: reply outcome unknown", "item", itemExternalID, "status", status, "err", err, "readback", ferr)
	return provider.Comment{}, fmt.Errorf("factorio: %w", ErrUnknownOutcome)
}

// submit posts the form once.
func (p *Provider) submit(ctx context.Context, path string, f replyForm, text string, jar *websession.Jar) (int, string, error) {
	target, err := url.Parse(p.opts.Site + path)
	if err != nil {
		return 0, "", err
	}
	if f.Action != "" {
		a, err := url.Parse(f.Action)
		if err != nil {
			return 0, "", err
		}
		target = target.ResolveReference(a)
	}
	var payload []byte
	h := http.Header{}
	if strings.Contains(strings.ToLower(f.Enctype), "multipart") {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, kv := range f.Fields {
			_ = w.WriteField(kv[0], kv[1])
		}
		_ = w.WriteField(f.TextName, text)
		_ = w.Close()
		payload = buf.Bytes()
		h.Set("Content-Type", w.FormDataContentType())
	} else {
		v := url.Values{}
		for _, kv := range f.Fields {
			v.Add(kv[0], kv[1])
		}
		v.Set(f.TextName, text)
		payload = []byte(v.Encode())
		h.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	h.Set("Referer", p.opts.Site+path)
	res, err := p.client(jar).Do(ctx, http.MethodPost, target.String(), h, payload, false)
	return res.Status, string(res.Body), err
}

// findReply re-reads the thread for the own new message.
func (p *Provider) findReply(ctx context.Context, mod, id, path string, jar *websession.Jar, m modkit.Match) (provider.Comment, bool, error) {
	page, err := p.get(ctx, path, jar)
	if err != nil {
		return provider.Comment{}, false, err
	}
	msgs, _, err := parseThread(page)
	if err != nil {
		return provider.Comment{}, false, err
	}
	it := threadItem(mod, row{ID: id}, msgs)
	for i := len(msgs) - 1; i >= 1; i-- {
		msg := msgs[i]
		if m.OK(msg.Author+"|"+msg.At, msg.Author, msg.Body, parseTime(msg.At)) {
			return it.Comments[i-1], true, nil
		}
	}
	return provider.Comment{}, false, nil
}
