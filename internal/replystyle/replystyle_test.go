package replystyle

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type fixtures struct {
	Source string `json:"source"`
	Slop   []struct {
		Kind  string `json:"kind"`
		Draft string `json:"draft"`
	} `json:"slop"`
	Good []struct {
		Kind  string `json:"kind"`
		Draft string `json:"draft"`
	} `json:"good"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	b, err := os.ReadFile("testdata/drafts.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCheckRefusesSlop(t *testing.T) {
	f := loadFixtures(t)
	for _, c := range f.Slop {
		if p := Check(c.Draft, Input{Kind: c.Kind, Source: f.Source, Limit: 999}); len(p) == 0 {
			t.Errorf("slop passed: %q", c.Draft)
		}
	}
}

func TestCheckPassesGood(t *testing.T) {
	f := loadFixtures(t)
	for _, c := range f.Good {
		if p := Check(c.Draft, Input{Kind: c.Kind, Source: f.Source, Limit: 999}); len(p) != 0 {
			t.Errorf("good draft refused: %q: %v", c.Draft, p)
		}
	}
}

func TestCheckLimitAndEmoji(t *testing.T) {
	if p := Check(strings.Repeat("a", 50), Input{Limit: 40}); len(p) != 1 || !strings.Contains(p[0], "limit is 40") {
		t.Fatalf("over limit: %v", p)
	}
	// The reporter used emoji: one back is fine.
	if p := Check("Thanks, glad it works! 👍", Input{Kind: KindFeedback, Source: "works great 👍"}); len(p) != 0 {
		t.Fatalf("emoji mirrored: %v", p)
	}
	if p := Check("  ", Input{}); len(p) != 1 {
		t.Fatalf("empty: %v", p)
	}
}

func TestFallbackPasses(t *testing.T) {
	for _, k := range []string{KindFeedback, KindSuggestion, KindQuestion, KindNeedsInfo, ""} {
		for _, lang := range []string{"en", "ru"} {
			fb := Fallback(k, lang)
			if p := Check(fb, Input{Kind: k, Limit: 999}); len(p) != 0 {
				t.Errorf("fallback %s/%s refused: %v", k, lang, p)
			}
		}
	}
	if Fallback(KindFeedback, "en") != "Thanks, glad it works for you!" {
		t.Fatal(Fallback(KindFeedback, "en"))
	}
}

func TestLanguageAndRules(t *testing.T) {
	if Language("спасибо за мод") != "ru" || Language("thanks a lot") != "en" {
		t.Fatal("language")
	}
	if !strings.Contains(Rules(KindFeedback), "Thanks, glad it works for you!") || !strings.Contains(Rules(""), "Happy gaming") {
		t.Fatal("rules")
	}
	if !NonBug(KindFeedback) || NonBug(KindNeedsInfo) {
		t.Fatal("NonBug")
	}
}
