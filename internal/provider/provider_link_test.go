package provider

import "testing"

func TestGitHubRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"Source: [url=https://github.com/UberMorgott/PhoenixPoint-Mod-PerkOracle]GitHub[/url]": "https://github.com/UberMorgott/PhoenixPoint-Mod-PerkOracle",
		"support: github.com/sponsors/me, code: https://github.com/me/tool.git":                "https://github.com/me/tool",
		"no link here": "",
	} {
		if got := GitHubRepoURL(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
