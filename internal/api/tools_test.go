package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/UberMorgott/issuewatcher/internal/tools"
)

type fakeTool struct {
	st  tools.Status
	err error
}

func (f *fakeTool) Tool() tools.Status { return f.st }
func (f *fakeTool) Provision(context.Context) (tools.Status, error) {
	if f.err == nil {
		f.st.State, f.st.Path = tools.Ready, `E:\IW\data\tools\`+f.st.Name
	}
	return f.st, f.err
}

func TestToolsStatusAndProvision(t *testing.T) {
	cmd := &fakeTool{st: tools.Status{Name: "steamcmd", State: tools.Missing}}
	dll := &fakeTool{st: tools.Status{Name: "steamworks", State: tools.Missing}, err: errors.New("no game")}
	busy := &fakeTool{st: tools.Status{Name: "busy", State: tools.Working}, err: tools.ErrBusy}
	s, err := New(t.Context(), Options{Assets: fstest.MapFS{"index.html": {Data: []byte(indexHTML)}}, Version: "test",
		Open: func(string) {}, Log: slog.New(slog.DiscardHandler), Tools: []Tool{cmd, dll, busy}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })

	post := func(name string) result {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, s.BaseURL()+"/api/tools/"+name+"/provision", nil)
		req.Header.Set("Authorization", "Bearer "+s.Token())
		return do(t, http.DefaultClient, req)
	}
	if r := post("steamcmd"); r.status != http.StatusOK {
		t.Fatalf("provision steamcmd: %d %s", r.status, r.body)
	}
	if r := post("steamworks"); r.status != http.StatusBadGateway {
		t.Fatalf("failed provisioning: %d %s", r.status, r.body)
	}
	if r := post("busy"); r.status != http.StatusConflict {
		t.Fatalf("busy: %d %s", r.status, r.body)
	}
	if r := post("nope"); r.status != http.StatusNotFound {
		t.Fatalf("unknown: %d %s", r.status, r.body)
	}
	r := get(t, http.DefaultClient, s.BaseURL()+"/api/tools", s.Token())
	var body struct {
		Tools []tools.Status `json:"tools"`
	}
	if err := json.Unmarshal([]byte(r.body), &body); err != nil || r.status != http.StatusOK || len(body.Tools) != 3 ||
		body.Tools[0].State != tools.Ready || body.Tools[1].State != tools.Missing {
		t.Fatalf("status: %d %s", r.status, r.body)
	}
}
