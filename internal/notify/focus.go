package notify

import "strings"

// DashboardTitleMarker starts the dashboard tab's document.title in every UI
// language ("IssueWatcher · Обзор"), so the browser window showing it can be
// found by title. Browsers put the active tab title first in the window title.
const DashboardTitleMarker = "IssueWatcher · "

// IsDashboardTitle reports whether a top-level window title belongs to a browser
// window whose active tab is the dashboard: "IssueWatcher · Обзор - Google
// Chrome", a flashing "● IssueWatcher · …", or an installed app window
// ("IssueWatcher - IssueWatcher · …"). An editor showing a folder named
// IssueWatcher ("main.go - IssueWatcher - Visual Studio Code") does not match.
func IsDashboardTitle(title string) bool {
	t := strings.TrimPrefix(title, "● ")
	return strings.HasPrefix(t, DashboardTitleMarker) || strings.HasPrefix(t, "IssueWatcher - "+DashboardTitleMarker)
}
