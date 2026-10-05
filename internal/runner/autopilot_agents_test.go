package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// The classify prompt carries the report, the candidates (untrusted, with
// their fix state) and the latest release; the reply prompt asks for the
// reporter's language, the version and each link exactly once.
func TestAutopilotAgentPrompts(t *testing.T) {
	in := store.JobInput{Number: 3, Title: "Вылет при сохранении", Body: "Игра падает", ProjectName: "o/r", Platform: "github", Author: "ivan"}
	task := classifyTask(promptInput{in: in}, release.ClassifyRequest{ItemID: 9, LatestVersion: "1.2.3",
		Candidates: []release.Candidate{{ItemID: 4, Platform: "nexus", Number: 2, Title: "Crash on save", Open: true, Status: "fixing"}}})
	for _, want := range []string{"item 9", "latest released version is 1.2.3", "item 4: nexus #2 (open, fixing) Crash on save",
		`<untrusted-issue-content source="candidates">`, "Игра падает", "language:"} {
		if !strings.Contains(task, want) {
			t.Errorf("classify task lacks %q:\n%s", want, task)
		}
	}
	reply := draftTask(promptInput{in: in}, release.DraftRequest{Kind: release.DraftReleased, Version: "1.2.4", Language: "ru",
		Changes: []string{"fix: save crash"}, Links: []release.Link{{Name: "Nexus Mods", URL: "https://n/1"}}})
	for _, want := range []string{"language the reporter wrote in (triage detected: ru)", "version 1.2.4", "Nexus Mods: https://n/1",
		"each exactly once", "fix: save crash"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply task lacks %q:\n%s", want, reply)
		}
	}
	for _, flow := range []string{flowClassify, flowAutoReply} {
		if !json.Valid([]byte(schemas[flow])) {
			t.Errorf("schema %s is not JSON", flow)
		}
	}
	var res AgentResult
	applyStructured(`{"kind":"bug","duplicateOf":0,"actionable":true,"regression":false,"language":"ru","reason":"x","severity":"high","missing":""}`, &res)
	var c release.Classification
	if err := json.Unmarshal([]byte(res.raw), &c); err != nil || c.Kind != "bug" || c.Language != "ru" || !c.Actionable {
		t.Fatalf("classification from the raw output: %v %+v", err, c)
	}
}
