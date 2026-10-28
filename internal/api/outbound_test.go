package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/outbound"
)

type fakeOutbound struct{ calls []outbound.Request }

func (f *fakeOutbound) Do(_ context.Context, req outbound.Request) (outbound.Result, error) {
	f.calls = append(f.calls, req)
	switch req.URL {
	case "https://unknown.test/":
		return outbound.Result{}, &outbound.Error{Code: outbound.CodeUnsupported, Msg: "no platform"}
	case "https://lost.test/":
		return outbound.Result{}, &outbound.Error{Code: outbound.CodeOutcomeUnknown, Msg: "unknown"}
	}
	code := outbound.CodeOK
	if req.Verb == outbound.VerbPost || req.Verb == outbound.VerbReply {
		code = outbound.CodePosted
		if req.DryRun {
			code = outbound.CodeDryRun
		}
	}
	return outbound.Result{OK: true, Code: code, Verb: req.Verb, Provider: "github"}, nil
}

func TestOutboundStatusesAndAgentRefusal(t *testing.T) {
	ob := &fakeOutbound{}
	e, _ := releaseEnv(t, func(o *Options) { o.Outbound = ob })
	var out outbound.Result
	if code := e.call(t, http.MethodPost, "/api/outbound/post", `{"url":"https://github.com/o/r/issues/1","body":"x","dryRun":true}`, &out); code != http.StatusOK || out.Code != outbound.CodeDryRun {
		t.Fatalf("dry run %d %+v", code, out)
	}
	if code := e.call(t, http.MethodPost, "/api/outbound/reply", `{"url":"https://github.com/o/r/issues/1","body":"x"}`, &out); code != http.StatusCreated || out.Code != outbound.CodePosted {
		t.Fatalf("post %d %+v", code, out)
	}
	if code, c := e.callErr(t, http.MethodPost, "/api/outbound/read", `{"url":"https://unknown.test/"}`); code != http.StatusConflict || c != outbound.CodeUnsupported {
		t.Fatalf("unsupported %d %q", code, c)
	}
	if code, c := e.callErr(t, http.MethodPost, "/api/outbound/post", `{"url":"https://lost.test/","body":"x"}`); code != http.StatusBadGateway || c != outbound.CodeOutcomeUnknown {
		t.Fatalf("outcome unknown %d %q", code, c)
	}

	// Agent runs: writes only as dry runs; reads pass.
	e.s.inAgentJob = func(uint32) bool { return true }
	n := len(ob.calls)
	for _, verb := range []string{"post", "reply"} {
		if code, c := e.callErr(t, http.MethodPost, "/api/outbound/"+verb, `{"url":"https://github.com/o/r/issues/1","body":"x"}`); code != http.StatusForbidden || c != codeAgentCaller {
			t.Errorf("agent %s: %d %q", verb, code, c)
		}
	}
	if len(ob.calls) != n {
		t.Fatalf("an agent write reached the service: %+v", ob.calls[n:])
	}
	if code := e.call(t, http.MethodPost, "/api/outbound/post", `{"url":"https://github.com/o/r/issues/1","body":"x","dryRun":true}`, &out); code != http.StatusOK {
		t.Fatalf("agent dry run %d", code)
	}
	if code := e.call(t, http.MethodPost, "/api/outbound/read", `{"url":"https://github.com/o/r/issues/1"}`, &out); code != http.StatusOK || out.Code != outbound.CodeOK {
		t.Fatalf("agent read %d %+v", code, out)
	}
}
