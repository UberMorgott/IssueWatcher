package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// v7 → v8: an old file loads, autopilot defaults are filled, nothing else moves.
func TestAutopilotMigrationV8(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 7, "future": "keep", "agents": {"maxParallel": 3,
		"projects": {"github:octo/app": {"mode": "worktree-pr", "verify": "go test ./...", "future": 1},
			"github:octo/lib": {"autopilot": {"enabled": true, "autoTriage": false, "coalesceMinutes": 30}}}}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get().Agents
	if a.Autopilot != (AgentsAutopilot{MaxReleasesPerDay: 5, MaxPublishesPerDay: 20}) {
		t.Fatalf("global: %+v", a.Autopilot)
	}
	if got := a.AutopilotFor("github:octo/app"); !reflect.DeepEqual(got, DefaultProjectAutopilot()) {
		t.Fatalf("project defaults: %+v", got)
	}
	if got := a.AutopilotFor("github:octo/none"); !reflect.DeepEqual(got, DefaultProjectAutopilot()) {
		t.Fatalf("absent project: %+v", got)
	}
	want := DefaultProjectAutopilot()
	want.Enabled, want.AutoTriage, want.CoalesceMinutes = true, false, 30
	if got := a.AutopilotFor("github:octo/lib"); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial block: %+v", got)
	}
	if a.MaxParallel != 3 || a.ModeFor("github:octo/app") != ModeWorktreePR || a.Projects["github:octo/app"].Verify != "go test ./..." {
		t.Fatalf("agents: %+v", a)
	}
	d := DefaultProjectAutopilot()
	if d.Enabled || d.AutoFix || d.AutoPush || d.AutoRelease || d.AutoReply || d.PublishWithoutSmoke ||
		!d.AutoTriage || !d.GithubRelease || !d.AutoClose || d.CoalesceMinutes != 60 || d.MaxBatchAgeHours != 24 ||
		d.MaxReleasesPerDay != 2 || d.MaxDiffLines != 400 || d.RegressionWindowHrs != 48 {
		t.Fatalf("doc defaults: %+v", d)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	gl, _ := ag["autopilot"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	app, _ := projects["github:octo/app"].(map[string]any)
	if raw["schemaVersion"] != float64(8) || raw["future"] != "keep" || app["future"] != float64(1) || app["verify"] != "go test ./..." ||
		gl["paused"] != false || gl["maxReleasesPerDay"] != float64(5) || gl["maxPublishesPerDay"] != float64(20) {
		t.Fatalf("file: %v", raw)
	}
	for _, k := range []string{"autopilot", "publishProfile"} {
		if _, ok := app[k]; ok {
			t.Fatalf("default %s written: %v", k, app)
		}
	}
}

// Unknown keys inside the autopilot blocks survive a rewrite of known ones.
func TestAutopilotUnknownKeysKept(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 8, "agents": {"autopilot": {"future": "g"}, "projects": {"github:octo/app": {
		"autopilot": {"enabled": true, "future": "a", "publish": {"nexus:game/1": true}},
		"publishProfile": {"future": "p", "build": {"command": "pwsh -File build.ps1", "output": "dist/x.zip", "future": 2},
			"version": {"kind": "factorio-info", "future": 3},
			"targets": {"nexus:game/1": {"fileId": "9", "future": 4}}}}}}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Patch(0, json.RawMessage(`{"agents": {"autopilot": {"paused": true}, "projects": {"github:octo/app": {
		"autopilot": {"maxDiffLines": 100}, "publishProfile": {"changelog": {"kind": "factorio"}, "targets": {"nexus:game/1": {"category": "main"}}}}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Agents.Projects["github:octo/app"]
	if !got.Agents.Autopilot.Paused || !p.Autopilot.Enabled || p.Autopilot.MaxDiffLines != 100 || !p.Autopilot.Publish["nexus:game/1"] ||
		p.PublishProfile.Build.Command == "" || p.PublishProfile.Changelog.Kind != ChangelogFactorio ||
		!reflect.DeepEqual(p.PublishProfile.Targets["nexus:game/1"], TargetProfile{FileID: "9", Category: "main"}) {
		t.Fatalf("decoded: %+v", got.Agents)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	gl, _ := ag["autopilot"].(map[string]any)
	app, _ := ag["projects"].(map[string]any)["github:octo/app"].(map[string]any)
	ap, _ := app["autopilot"].(map[string]any)
	pp, _ := app["publishProfile"].(map[string]any)
	build, _ := pp["build"].(map[string]any)
	ver, _ := pp["version"].(map[string]any)
	tgt, _ := pp["targets"].(map[string]any)["nexus:game/1"].(map[string]any)
	if gl["future"] != "g" || ap["future"] != "a" || pp["future"] != "p" || build["future"] != float64(2) || ver["future"] != float64(3) ||
		tgt["future"] != float64(4) || tgt["category"] != "main" || ap["maxDiffLines"] != float64(100) {
		t.Fatalf("file: %v", raw)
	}
}

func TestAutopilotValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := func(block string) string { return `{"agents": {"projects": {"github:o/r": {` + block + `}}}}` }
	const base = "agents.projects.github:o/r."
	cases := map[string][2]string{
		`{"agents": {"autopilot": {"maxReleasesPerDay": 0}}}`:                                           {"agents.autopilot.maxReleasesPerDay", "range"},
		`{"agents": {"autopilot": {"maxPublishesPerDay": 999}}}`:                                        {"agents.autopilot.maxPublishesPerDay", "range"},
		proj(`"autopilot": {"coalesceMinutes": 0}`):                                                     {base + "autopilot.coalesceMinutes", "range"},
		proj(`"autopilot": {"maxBatchAgeHours": 1000}`):                                                 {base + "autopilot.maxBatchAgeHours", "range"},
		proj(`"autopilot": {"maxReleasesPerDay": 21}`):                                                  {base + "autopilot.maxReleasesPerDay", "range"},
		proj(`"autopilot": {"maxDiffLines": -1}`):                                                       {base + "autopilot.maxDiffLines", "range"},
		proj(`"autopilot": {"regressionWindowHours": 0}`):                                               {base + "autopilot.regressionWindowHours", "range"},
		proj(`"autopilot": {"publish": {"nokey": true}}`):                                               {base + "autopilot.publish.nokey", "projectKey"},
		proj(`"publishProfile": {"version": {"kind": "toml"}}`):                                         {base + "publishProfile.version.kind", "enum"},
		proj(`"publishProfile": {"version": {"kind": "json", "path": "a.json"}}`):                       {base + "publishProfile.version.key", "required"},
		proj(`"publishProfile": {"version": {"kind": "regex", "path": "a", "pattern": "v(\\d)(\\d)"}}`): {base + "publishProfile.version.pattern", "regexGroup"},
		proj(`"publishProfile": {"version": {"kind": "regex", "path": "a", "pattern": "("}}`):           {base + "publishProfile.version.pattern", "regex"},
		proj(`"publishProfile": {"version": {"kind": "json", "path": "../x.json", "key": "v"}}`):        {base + "publishProfile.version.path", "repoPath"},
		proj(`"publishProfile": {"changelog": {"kind": "news"}}`):                                       {base + "publishProfile.changelog.kind", "enum"},
		proj(`"publishProfile": {"changelog": {"kind": "keepachangelog", "path": "C:/x.md"}}`):          {base + "publishProfile.changelog.path", "repoPath"},
		proj(`"publishProfile": {"smoke": {"kind": "steam"}}`):                                          {base + "publishProfile.smoke.kind", "enum"},
		proj(`"publishProfile": {"smoke": {"kind": "command"}}`):                                        {base + "publishProfile.smoke.command", "required"},
		proj(`"publishProfile": {"build": {"command": "make"}}`):                                        {base + "publishProfile.build.output", "required"},
		proj(`"publishProfile": {"build": {"command": "make", "output": "a.zip", "path": "b.zip"}}`):    {base + "publishProfile.build", "exclusive"},
		proj(`"publishProfile": {"targets": {"curseforge:1": {"releaseType": "nightly"}}}`):             {base + "publishProfile.targets.curseforge:1.releaseType", "enum"},
		proj(`"publishProfile": {"targets": {"bad": {}}}`):                                              {base + "publishProfile.targets.bad", "projectKey"},
		proj(`"publishProfile": {"targets": {"nexus:g/1": {"apiKey": "x"}}}`):                           {base + "publishProfile.targets.nexus:g/1.apiKey", "secret"},
		proj(`"autopilot": {"githubToken": "x"}`):                                                       {base + "autopilot.githubToken", "secret"},
	}
	for patch, want := range cases {
		_, err := s.Patch(0, json.RawMessage(patch), nil)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != want[0] || ve.Code != want[1] {
			t.Errorf("%s: %v, want %s/%s", patch, err, want[0], want[1])
		}
	}
	ok := proj(`"autopilot": {"enabled": true, "publish": {"factorio:my-mod": true}}, "publishProfile": {
		"build": {"command": "pwsh -File build.ps1", "output": "dist/{name}_{version}.zip"}, "steamContent": "dist/steam",
		"version": {"kind": "regex", "path": "src/version.go", "pattern": "Version = \"([^\"]+)\""}, "changelog": {"kind": "keepachangelog"},
		"smoke": {"kind": "factorio", "save": "E:/Saves/test.zip", "ticks": 600},
		"targets": {"nexus:wartales/202": {"fileId": "1", "category": "main", "archivePrevious": true}, "factorio:my-mod": {},
			"steam:3739613434": {"appId": 1234}, "curseforge:1443010": {"gameVersions": ["1.0"], "releaseType": "release"}}}`)
	if _, err := s.Patch(0, json.RawMessage(ok), nil); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
}
