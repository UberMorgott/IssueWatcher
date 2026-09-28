package notify

import "image/color"

// Theme is the popup palette. The app's settings feed it (Tray.SetTheme); the
// defaults mirror the dashboard tokens in frontend/src/style.css.
type Theme struct {
	Bg      color.NRGBA // card background
	Surface color.NRGBA // hover fill (close button)
	Border  color.NRGBA
	Text    color.NRGBA
	Muted   color.NRGBA // time, close cross
	Accent  color.NRGBA // repo#number, kind tile
	Success color.NRGBA // closed check
	Shadow  uint8       // drop shadow opacity
}

// DarkTheme is the default palette (dashboard dark tokens).
func DarkTheme() Theme {
	return Theme{
		Bg:      rgb(0x1a, 0x25, 0x32),
		Surface: rgb(0x29, 0x37, 0x47),
		Border:  rgb(0x3a, 0x4b, 0x5f),
		Text:    rgb(0xec, 0xf2, 0xf8),
		Muted:   rgb(0x96, 0xa6, 0xb8),
		Accent:  rgb(0x86, 0xa5, 0xff),
		Success: rgb(0x48, 0xc9, 0x92),
		Shadow:  150,
	}
}

// LightTheme mirrors the dashboard light tokens.
func LightTheme() Theme {
	return Theme{
		Bg:      rgb(0xff, 0xff, 0xff),
		Surface: rgb(0xe6, 0xec, 0xf3),
		Border:  rgb(0xd9, 0xe2, 0xec),
		Text:    rgb(0x17, 0x23, 0x31),
		Muted:   rgb(0x62, 0x73, 0x86),
		Accent:  rgb(0x36, 0x5f, 0xd3),
		Success: rgb(0x18, 0x83, 0x5d),
		Shadow:  60,
	}
}

func rgb(r, g, b uint8) color.NRGBA { return color.NRGBA{R: r, G: g, B: b, A: 0xff} }
