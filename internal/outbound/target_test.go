package outbound

import (
	"errors"
	"testing"
)

func TestResolvePerProvider(t *testing.T) {
	cases := []struct {
		name, url, parent string
		want              Target
	}{
		{"github issue", "https://github.com/UberMorgott/IssueWatcher/issues/12", "",
			Target{Platform: "github", Kind: KindIssue, Project: "UberMorgott/IssueWatcher", Number: 12, URL: "https://github.com/UberMorgott/IssueWatcher/issues/12"}},
		{"github issue comment", "https://www.github.com/o/r/issues/3#issuecomment-998877", "",
			Target{Platform: "github", Kind: KindIssue, Project: "o/r", Number: 3, Comment: "998877", URL: "https://github.com/o/r/issues/3"}},
		{"steam item", "https://steamcommunity.com/sharedfiles/filedetails/?id=3412345678&searchtext=", "",
			Target{Platform: "steam", Kind: KindWorkshopItem, Project: "3412345678", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=3412345678"}},
		{"steam comment fragment", "https://steamcommunity.com/workshop/filedetails/?id=42#comment_600123", "",
			Target{Platform: "steam", Kind: KindWorkshopItem, Project: "42", Comment: "600123", Thread: "comment:42/600123", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=42"}},
		{"steam parent id", "https://steamcommunity.com/sharedfiles/filedetails/?id=42", "777",
			Target{Platform: "steam", Kind: KindWorkshopItem, Project: "42", Comment: "777", Thread: "comment:42/777", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=42"}},
		{"nexus mod", "https://www.nexusmods.com/wartales/mods/202?tab=description", "",
			Target{Platform: "nexus", Kind: KindMod, Project: "wartales/202", URL: "https://www.nexusmods.com/wartales/mods/202"}},
		{"nexus comment", "https://www.nexusmods.com/wartales/mods/202?tab=posts&jump_to_comment=155001", "",
			Target{Platform: "nexus", Kind: KindThread, Project: "wartales/202", Thread: "comment:wartales/202/155001", Comment: "155001", URL: "https://www.nexusmods.com/wartales/mods/202"}},
		{"nexus bug parent", "https://www.nexusmods.com/wartales/mods/202", "bug:900",
			Target{Platform: "nexus", Kind: KindThread, Project: "wartales/202", Thread: "bug:900", Comment: "900", URL: "https://www.nexusmods.com/wartales/mods/202"}},
		{"nexus parent url", "https://www.nexusmods.com/wartales/mods/202", "https://www.nexusmods.com/wartales/mods/202?tab=posts&jump_to_comment=5",
			Target{Platform: "nexus", Kind: KindThread, Project: "wartales/202", Thread: "comment:wartales/202/5", Comment: "5", URL: "https://www.nexusmods.com/wartales/mods/202"}},
		{"factorio mod", "https://mods.factorio.com/mod/lazy-builder", "",
			Target{Platform: "factorio", Kind: KindMod, Project: "lazy-builder", URL: "https://mods.factorio.com/mod/lazy-builder"}},
		{"factorio discussion list", "https://mods.factorio.com/mod/lazy-builder/discussion", "",
			Target{Platform: "factorio", Kind: KindMod, Project: "lazy-builder", URL: "https://mods.factorio.com/mod/lazy-builder"}},
		{"factorio thread", "https://mods.factorio.com/mod/lazy-builder/discussion/65f0c1a2b3c4d5e6f7a8b9c0", "",
			Target{Platform: "factorio", Kind: KindThread, Project: "lazy-builder", Thread: "thread:65f0c1a2b3c4d5e6f7a8b9c0", URL: "https://mods.factorio.com/mod/lazy-builder/discussion/65f0c1a2b3c4d5e6f7a8b9c0"}},
		{"curseforge project", "https://www.curseforge.com/projects/1234567", "",
			Target{Platform: "curseforge", Kind: KindMod, Project: "1234567", URL: "https://www.curseforge.com/projects/1234567"}},
		{"curseforge parent", "https://www.curseforge.com/projects/1234567", "8800",
			Target{Platform: "curseforge", Kind: KindThread, Project: "1234567", Thread: "comment:1234567/8800", Comment: "8800", URL: "https://www.curseforge.com/projects/1234567"}},
	}
	for _, c := range cases {
		got, err := Resolve(c.url, c.parent)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v\nwant %+v", c.name, got, err, c.want)
		}
	}
}

func TestResolveRefusals(t *testing.T) {
	cases := []struct{ url, parent, code string }{
		{"not a url", "", CodeBadURL},
		{"ftp://github.com/o/r/issues/1", "", CodeBadURL},
		{"https://example.com/x", "", CodeUnsupported},
		{"https://github.com/o/r/pull/5", "", CodeUnsupported},
		{"https://github.com/o/r/issues/abc", "", CodeBadURL},
		{"https://github.com/o/r", "", CodeBadURL},
		{"https://steamcommunity.com/id/someone", "", CodeUnsupported},
		{"https://steamcommunity.com/sharedfiles/filedetails/?id=abc", "", CodeBadURL},
		{"https://steamcommunity.com/sharedfiles/filedetails/?id=42", "x1", CodeBadURL},
		{"https://www.nexusmods.com/wartales/mods/abc", "", CodeBadURL},
		{"https://www.nexusmods.com/wartales/mods/202", "bug:x", CodeBadURL},
		{"https://mods.factorio.com/mod/x/discussion/nothex", "", CodeBadURL},
		{"https://www.curseforge.com/minecraft/mc-mods/jei", "", CodeUnsupported},
		{"https://www.curseforge.com/projects/1", "abc", CodeBadURL},
	}
	for _, c := range cases {
		_, err := Resolve(c.url, c.parent)
		var e *Error
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%s (%q): %v, want code %s", c.url, c.parent, err, c.code)
		}
	}
}
