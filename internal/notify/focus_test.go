package notify

import "testing"

func TestIsDashboardTitle(t *testing.T) {
	for title, want := range map[string]bool{
		"IssueWatcher · Обзор - Google Chrome":                      true,
		"IssueWatcher · Issues — Mozilla Firefox":                   true,
		"IssueWatcher · Settings and 3 more pages - Microsoft Edge": true,
		"● IssueWatcher · Issues - Google Chrome":                   true,
		"IssueWatcher - IssueWatcher · Обзор":                       true,
		"main.go - IssueWatcher - Visual Studio Code":               false,
		"IssueWatcher - Google Chrome":                              false,
		"GitHub · IssueWatcher - Google Chrome":                     false,
		"":                                                          false,
	} {
		if got := IsDashboardTitle(title); got != want {
			t.Errorf("IsDashboardTitle(%q) = %v, want %v", title, got, want)
		}
	}
}
