package steam

import (
	"testing"
)

// The card shows the profile's public name: read from the profile XML during
// a sync, cached in the settings, dropped when the SteamID changes.
func TestPersonaCachedAndReset(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	if _, err := p.ListProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := p.Status()
	if st.Persona != "Morgott" || p.Persona() != "Morgott" {
		t.Fatalf("persona %q", st.Persona)
	}
	if _, err := p.ListProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("/profiles/" + ownerID + "/"); n != 1 {
		t.Fatalf("profile xml read %d times, want 1 (cached)", n)
	}
	other := "76561197960287930"
	if st, _ = p.Save(Update{SteamID: &other}); st.Persona != "" {
		t.Fatalf("persona kept for another SteamID: %q", st.Persona)
	}
}

// «Выйти» drops the cookies and the QR refresh token but keeps the SteamID:
// the public reads go on.
func TestLogoutKeepsSteamID(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	sec, sid := "76561197996210591%7C%7Ctoken", "abc123"
	if _, err := p.Save(Update{LoginSecure: &sec, SessionID: &sid}); err != nil {
		t.Fatal(err)
	}
	if err := p.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := p.Status()
	if st.HasCookies || st.SignedIn || st.Session != SessionNone || st.SteamID != ownerID {
		t.Fatalf("after logout %+v", st)
	}
	if p.Capabilities().Reply {
		t.Fatal("reply still on after logout")
	}
}
