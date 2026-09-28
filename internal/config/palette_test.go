package config

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestPalettesAreReadable(t *testing.T) {
	if len(palettes) < 4 || len(palettes) > 6 {
		t.Fatalf("%d palettes, want 4–6", len(palettes))
	}
	for _, p := range palettes {
		for mode, c := range map[string]Colors{"dark": p.Dark, "light": p.Light} {
			for _, h := range []string{c.Accent, c.Background, c.Surface, c.Text} {
				if !isHex(h) {
					t.Fatalf("%s/%s: bad hex %q", p.ID, mode, h)
				}
			}
			if r := Contrast(c.Text, c.Surface); r < 7 {
				t.Errorf("%s/%s: text on surface %.1f:1, presets aim for AAA (7:1)", p.ID, mode, r)
			}
			if r := Contrast(c.Text, c.Background); r < 7 {
				t.Errorf("%s/%s: text on background %.1f:1", p.ID, mode, r)
			}
			if r := Contrast(c.Accent, c.Surface); r < 3 {
				t.Errorf("%s/%s: accent on surface %.1f:1, want ≥ 3 (UI components)", p.ID, mode, r)
			}
		}
	}
}

func TestContrast(t *testing.T) {
	if r := Contrast("#000000", "#ffffff"); r < 20.9 || r > 21.1 {
		t.Fatalf("black/white %.2f", r)
	}
	if r := Contrast("#777777", "#777777"); r != 1 {
		t.Fatalf("same %.2f", r)
	}
}

func TestAppearanceValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	good := `{"appearance": {"paletteId": "ocean", "fontFamily": "segoe", "fontScale": 1.15, "density": "compact",
		"custom": {"dark": {"accent": "#FF8800"}}}}`
	got, err := s.Patch(0, json.RawMessage(good), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := got.Appearance.Effective("dark"); e.Accent != "#ff8800" || e.Background != "#071316" {
		t.Fatalf("effective %+v", e)
	}
	if e := got.Appearance.Effective("light"); e.Accent != "#0b7479" {
		t.Fatalf("light untouched by the dark override: %+v", e)
	}
	cases := map[string]struct{ field, code string }{
		`{"appearance": {"paletteId": "neon"}}`:                         {"appearance.paletteId", "enum"},
		`{"appearance": {"fontScale": 2}}`:                              {"appearance.fontScale", "range"},
		`{"appearance": {"density": "huge"}}`:                           {"appearance.density", "enum"},
		`{"appearance": {"fontFamily": "comic"}}`:                       {"appearance.fontFamily", "enum"},
		`{"appearance": {"custom": {"light": {"accent": "red"}}}}`:      {"appearance.custom.light.accent", "hex"},
		`{"appearance": {"custom": {"dark": {"text": "#1a1d1f"}}}}`:     {"appearance.custom.dark.text", "contrast"},
		`{"appearance": {"custom": {"light": {"surface": "#222222"}}}}`: {"appearance.custom.light.text", "contrast"},
		`{"appearance": {"custom": {"light": {"text": "#999999"}}}}`:    {"appearance.custom.light.text", "contrast"},
	}
	for patch, want := range cases {
		_, err := s.Patch(1, json.RawMessage(patch), nil)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != want.field || ve.Code != want.code {
			t.Errorf("%s: %v, want %s/%s", patch, err, want.field, want.code)
		}
	}
	// Clearing a custom colour ("") falls back to the palette.
	got, err = s.Patch(1, json.RawMessage(`{"appearance": {"custom": {"dark": {"accent": ""}}}}`), nil)
	if err != nil || got.Appearance.Effective("dark").Accent != "#46c8cc" {
		t.Fatalf("reset custom: %v %+v", err, got.Appearance.Effective("dark"))
	}
}
