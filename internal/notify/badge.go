// Package notify renders the tray icon and shows popup notifications.
package notify

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
)

const iconSize = 32

var (
	colorBase   = color.RGBA{R: 0x25, G: 0x63, B: 0xeb, A: 0xff} // blue
	colorMark   = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	colorBadge  = color.RGBA{R: 0xdc, G: 0x26, B: 0x26, A: 0xff} // red
	colorDigits = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
)

// glyphs is a 3x5 pixel font; each row is 3 bits, MSB = left column.
var glyphs = map[rune][5]uint8{
	'0': {7, 5, 5, 5, 7},
	'1': {2, 6, 2, 2, 7},
	'2': {7, 1, 7, 4, 7},
	'3': {7, 1, 7, 1, 7},
	'4': {5, 5, 7, 1, 1},
	'5': {7, 4, 7, 1, 7},
	'6': {7, 4, 7, 5, 7},
	'7': {7, 1, 1, 1, 1},
	'8': {7, 5, 7, 5, 7},
	'9': {7, 5, 7, 1, 7},
	'+': {0, 2, 7, 2, 0},
}

// BadgeLabel returns the text drawn on the badge: "" for none, "99+" above 99.
func BadgeLabel(count int) string {
	switch {
	case count <= 0:
		return ""
	case count > 99:
		return "99+"
	default:
		return strconv.Itoa(count)
	}
}

// TrayIcon renders the tray icon PNG with a count badge (no badge when count <= 0).
func TrayIcon(count int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))

	// Base: rounded blue square with a white ring ("watch" eye).
	fillRoundRect(img, image.Rect(1, 1, iconSize-1, iconSize-1), 7, colorBase)
	fillRing(img, iconSize/2, iconSize/2, 9, 5, colorMark)

	if label := BadgeLabel(count); label != "" {
		drawBadge(img, label)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("notify: encode tray icon: %w", err)
	}
	return buf.Bytes(), nil
}

// drawBadge draws a red pill in the bottom-right corner with label in 2x-scaled glyphs.
func drawBadge(img *image.RGBA, label string) {
	const scale, pad = 2, 2
	glyphW, glyphH := 3*scale, 5*scale
	textW := len(label)*glyphW + (len(label)-1)*scale
	h := glyphH + 2*pad
	w := max(textW+2*pad, h)

	pill := image.Rect(iconSize-w, iconSize-h, iconSize, iconSize)
	fillRoundRect(img, pill, h/2, colorBadge)

	x := pill.Min.X + (w-textW)/2
	y := pill.Min.Y + pad
	for _, r := range label {
		rows := glyphs[r]
		for row := range 5 {
			for col := range 3 {
				if rows[row]&(4>>col) == 0 {
					continue
				}
				for dy := range scale {
					for dx := range scale {
						img.SetRGBA(x+col*scale+dx, y+row*scale+dy, colorDigits)
					}
				}
			}
		}
		x += glyphW + scale
	}
}

func fillRoundRect(img *image.RGBA, r image.Rectangle, radius int, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if insideRounded(x, y, r, radius) {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// insideRounded reports whether pixel (x, y) lies in r with corners of the given radius.
func insideRounded(x, y int, r image.Rectangle, radius int) bool {
	// Pixel centers, doubled to stay in integers.
	px, py := 2*x+1, 2*y+1
	cx := clamp(px, 2*(r.Min.X+radius), 2*(r.Max.X-radius))
	cy := clamp(py, 2*(r.Min.Y+radius), 2*(r.Max.Y-radius))
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= 4*radius*radius
}

func fillRing(img *image.RGBA, cx, cy, outer, inner int, c color.RGBA) {
	for y := cy - outer; y < cy+outer; y++ {
		for x := cx - outer; x < cx+outer; x++ {
			dx, dy := 2*(x-cx)+1, 2*(y-cy)+1
			d := dx*dx + dy*dy
			if d <= 4*outer*outer && d >= 4*inner*inner {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
