package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Colors are the four base colours of one theme mode; everything else
// (borders, muted text, hover, the accent scale) is derived from them in the
// UI, so a palette or a custom edit stays coherent.
type Colors struct {
	Accent     string `json:"accent"`
	Background string `json:"background"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
}

// Palette is a preset: a light and a dark variant.
type Palette struct {
	ID    string `json:"id"`
	Dark  Colors `json:"dark"`
	Light Colors `json:"light"`
}

// Palettes are the presets, first = default. The dark/light values of
// "indigo" are the original dashboard tokens (frontend/src/style.css).
var palettes = []Palette{
	{ID: "indigo",
		Dark:  Colors{Accent: "#86a5ff", Background: "#0b1017", Surface: "#121a24", Text: "#ecf2f8"},
		Light: Colors{Accent: "#365fd3", Background: "#f5f7fa", Surface: "#ffffff", Text: "#172331"}},
	{ID: "ocean",
		Dark:  Colors{Accent: "#46c8cc", Background: "#071316", Surface: "#0e1d21", Text: "#e4f3f4"},
		Light: Colors{Accent: "#0b7479", Background: "#f1f7f7", Surface: "#ffffff", Text: "#0f2a2e"}},
	{ID: "forest",
		Dark:  Colors{Accent: "#62cf8c", Background: "#0a110c", Surface: "#111b14", Text: "#e8f2ea"},
		Light: Colors{Accent: "#1d7443", Background: "#f3f7f3", Surface: "#ffffff", Text: "#15251a"}},
	{ID: "amber",
		Dark:  Colors{Accent: "#f2b45c", Background: "#13100a", Surface: "#1c1810", Text: "#f5eee2"},
		Light: Colors{Accent: "#94570b", Background: "#faf6ef", Surface: "#ffffff", Text: "#291f12"}},
	{ID: "rose",
		Dark:  Colors{Accent: "#f27fa9", Background: "#140b10", Surface: "#1e1318", Text: "#f6e9ef"},
		Light: Colors{Accent: "#b12d5c", Background: "#fbf4f7", Surface: "#ffffff", Text: "#2a161f"}},
	{ID: "graphite",
		Dark:  Colors{Accent: "#b8bec9", Background: "#0d0e10", Surface: "#16171a", Text: "#ececee"},
		Light: Colors{Accent: "#3d424b", Background: "#f5f5f6", Surface: "#ffffff", Text: "#1a1b1e"}},
}

// Palettes returns the presets (for the UI).
func Palettes() []Palette { return slices.Clone(palettes) }

func palette(id string) (Palette, bool) {
	i := slices.IndexFunc(palettes, func(p Palette) bool { return p.ID == id })
	if i < 0 {
		return Palette{}, false
	}
	return palettes[i], true
}

// Font families the UI offers.
var fontFamilies = []string{"inter", "segoe", "system", "mono"}

// Table densities.
var densities = []string{"compact", "comfortable", "spacious"}

// Font scale bounds.
const (
	MinFontScale = 0.85
	MaxFontScale = 1.30
)

// MinContrast is WCAG AA for body text.
const MinContrast = 4.5

// Effective resolves the colours of mode ("dark" | "light") for appearance a:
// the palette, overridden by the non-empty custom colours of that mode.
func (a Appearance) Effective(mode string) Colors {
	p, ok := palette(a.PaletteID)
	if !ok {
		p = palettes[0]
	}
	base, custom := p.Dark, a.Custom.Dark
	if mode == "light" {
		base, custom = p.Light, a.Custom.Light
	}
	for _, f := range []struct{ dst, src *string }{
		{&base.Accent, &custom.Accent}, {&base.Background, &custom.Background},
		{&base.Surface, &custom.Surface}, {&base.Text, &custom.Text},
	} {
		if *f.src != "" {
			*f.dst = strings.ToLower(*f.src)
		}
	}
	return base
}

func isHex(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	_, err := strconv.ParseUint(s[1:], 16, 32)
	return err == nil
}

// luminance is the WCAG relative luminance of #rrggbb.
func luminance(hex string) float64 {
	v, _ := strconv.ParseUint(hex[1:], 16, 32)
	ch := func(c uint64) float64 {
		s := float64(c) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(v>>16&0xff) + 0.7152*ch(v>>8&0xff) + 0.0722*ch(v&0xff)
}

// Contrast is the WCAG contrast ratio of two #rrggbb colours (1–21).
func Contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func (a Appearance) validate() error {
	if !slices.Contains([]string{"dark", "light", "system"}, a.Mode) {
		return notOneOf("appearance.mode", "dark", "light", "system")
	}
	if _, ok := palette(a.PaletteID); !ok {
		ids := make([]string, len(palettes))
		for i, p := range palettes {
			ids[i] = p.ID
		}
		return notOneOf("appearance.paletteId", ids...)
	}
	if !slices.Contains(fontFamilies, a.FontFamily) {
		return notOneOf("appearance.fontFamily", fontFamilies...)
	}
	if math.IsNaN(a.FontScale) || a.FontScale < MinFontScale || a.FontScale > MaxFontScale {
		return invalid("appearance.fontScale", "range", map[string]any{"min": MinFontScale, "max": MaxFontScale},
			"must be %.2f–%.2f", MinFontScale, MaxFontScale)
	}
	if !slices.Contains(densities, a.Density) {
		return notOneOf("appearance.density", densities...)
	}
	for _, m := range []struct {
		mode string
		c    Colors
	}{{"dark", a.Custom.Dark}, {"light", a.Custom.Light}} {
		for name, v := range map[string]string{"accent": m.c.Accent, "background": m.c.Background, "surface": m.c.Surface, "text": m.c.Text} {
			if v != "" && !isHex(v) {
				return invalid("appearance.custom."+m.mode+"."+name, "hex", nil, "must be #RRGGBB")
			}
		}
		eff := a.Effective(m.mode)
		for _, bg := range []struct{ name, hex string }{{"surface", eff.Surface}, {"background", eff.Background}} {
			if r := Contrast(eff.Text, bg.hex); r < MinContrast {
				return invalid("appearance.custom."+m.mode+".text", "contrast",
					map[string]any{"ratio": fmt.Sprintf("%.1f", math.Floor(r*10)/10), "min": MinContrast, "on": bg.name, "mode": m.mode},
					"text on %s (%s) has contrast %.1f:1, below %.1f:1", bg.name, m.mode, r, MinContrast)
			}
		}
	}
	return nil
}
