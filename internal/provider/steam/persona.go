package steam

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// Persona is the cached public display name of the SteamID ("" = not known yet).
func (p *Provider) Persona() string {
	s, _ := p.current()
	return s.Persona
}

// RefreshPersona reads the profile's public display name (keyless profile XML,
// steamcommunity.com/profiles/<id>/?xml=1) and caches it in the settings.
func (p *Provider) RefreshPersona(ctx context.Context) (string, error) {
	s, err := p.current()
	if err != nil || s.SteamID == "" {
		return "", err
	}
	body, err := p.get(ctx, p.opts.CommunityURL+"/profiles/"+s.SteamID+"/?xml=1")
	if err != nil {
		return "", err
	}
	var r struct {
		SteamID64 string `xml:"steamID64"`
		Name      string `xml:"steamID"`
		Error     string `xml:"error"`
	}
	if err := xml.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("steam: profile xml: %w", err)
	}
	name := strings.TrimSpace(r.Name)
	if r.Error != "" || name == "" || (r.SteamID64 != "" && r.SteamID64 != s.SteamID) {
		return "", fmt.Errorf("steam: profile xml: no name (%s)", cmpStr(r.Error, "empty"))
	}
	p.save.Lock()
	defer p.save.Unlock()
	p.mu.Lock()
	cur := p.settings
	if cur.SteamID != s.SteamID || cur.Persona == name {
		p.mu.Unlock()
		return name, nil // another profile saved meanwhile, or unchanged
	}
	cur.Persona = name
	p.settings = cur
	p.mu.Unlock()
	if err := saveSettings(p.opts.Dir, cur); err != nil {
		return name, err
	}
	return name, nil
}

// Logout drops the web session (cookies and the QR refresh token); the
// SteamID stays, so the public reads go on (read-only).
func (p *Provider) Logout(context.Context) error {
	p.CancelQR()
	empty := ""
	_, err := p.Save(Update{LoginSecure: &empty, SessionID: &empty})
	return err
}
