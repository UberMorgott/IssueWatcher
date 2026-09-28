package notify

import (
	_ "embed" // Inter fonts (SIL OFL 1.1, fonts/OFL.txt)
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	//go:embed fonts/Inter-Regular.ttf
	interRegular []byte
	//go:embed fonts/Inter-SemiBold.ttf
	interSemiBold []byte

	fontsOnce               sync.Once
	fontRegular, fontStrong *opentype.Font
	errFonts                error
)

func loadFonts() error {
	fontsOnce.Do(func() {
		if fontRegular, errFonts = opentype.Parse(interRegular); errFonts != nil {
			return
		}
		fontStrong, errFonts = opentype.Parse(interSemiBold)
	})
	if errFonts != nil {
		return fmt.Errorf("notify: font: %w", errFonts)
	}
	return nil
}

// Card geometry in logical pixels (96 DPI); the renderer multiplies by scale.
const (
	cardWidth   = 360.0
	shadowPad   = 18.0 // transparent margin around the body for the shadow
	cardRadius  = 12.0
	cardInset   = 14.0
	tileSize    = 36.0
	textLeft    = cardInset + tileSize + 12
	closeCX     = cardWidth - 20 // close button centre, from the body's top-left
	closeCY     = 20.0
	closeRadius = 12.0
	timeRight   = cardWidth - 38 // right edge of the time text
	titleBase   = cardInset + 14 // baselines
	refBase     = titleBase + 19
	snippetBase = refBase + 20
	lineStep    = 18.0
	maxLines    = 2
)

// Hit zones of a card window.
const (
	hitNone = iota
	hitBody
	hitClose
)

type faces struct{ title, small, body font.Face }

func newFaces(scale float64) (faces, error) {
	if err := loadFonts(); err != nil {
		return faces{}, err
	}
	mk := func(f *opentype.Font, size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size * scale, DPI: 72, Hinting: font.HintingNone})
	}
	var fs faces
	var err error
	if fs.title, err = mk(fontStrong, 14); err != nil {
		return faces{}, fmt.Errorf("notify: face: %w", err)
	}
	if fs.small, err = mk(fontRegular, 12); err != nil {
		return faces{}, fmt.Errorf("notify: face: %w", err)
	}
	if fs.body, err = mk(fontRegular, 13); err != nil {
		return faces{}, fmt.Errorf("notify: face: %w", err)
	}
	return fs, nil
}

// cardLayout is the text of a card fitted to its width at one scale.
type cardLayout struct {
	title, ref, time string
	lines            []string
	bodyH            float64 // logical px
}

func layoutCard(c Card, fs faces, scale float64) cardLayout {
	l := cardLayout{ref: c.Ref}
	if !c.Time.IsZero() {
		l.time = c.Time.Format("15:04")
	}
	timeW := float64(font.MeasureString(fs.small, l.time)) / 64 / scale
	l.title = ellipsize(fs.title, c.Title, toFixed((timeRight-timeW-8-textLeft)*scale))
	width := toFixed((cardWidth - textLeft - cardInset) * scale)
	l.ref = ellipsize(fs.small, c.Ref, width)
	if c.Kind == KindGroup { // summary sits on the ref line
		l.ref = ellipsize(fs.small, c.Text, width)
	} else {
		l.lines = wrapText(fs.body, c.Text, width, maxLines)
	}
	last := refBase
	if n := len(l.lines); n > 0 {
		last = snippetBase + float64(n-1)*lineStep
	}
	l.bodyH = math.Max(last+5+cardInset, tileSize+2*cardInset)
	return l
}

func toFixed(v float64) fixed.Int26_6 { return fixed.Int26_6(math.Round(v * 64)) }

// wrapText breaks s into at most maxLines lines no wider than maxW; overflow
// ends the last line with "…"; a word wider than a line is split by runes.
func wrapText(face font.Face, s string, maxW fixed.Int26_6, maxLines int) []string {
	words := strings.Fields(s)
	var lines []string
	for len(words) > 0 && len(lines) < maxLines {
		if len(lines) == maxLines-1 { // last line takes the rest
			return append(lines, ellipsize(face, strings.Join(words, " "), maxW))
		}
		line, n := "", 0
		for n < len(words) {
			cand := words[n]
			if line != "" {
				cand = line + " " + words[n]
			}
			if font.MeasureString(face, cand) > maxW {
				break
			}
			line, n = cand, n+1
		}
		if n == 0 { // first word alone is too wide: split it
			head := fitRunes(face, words[0], maxW)
			line = head
			words[0] = strings.TrimPrefix(words[0], head)
		} else {
			words = words[n:]
		}
		lines = append(lines, line)
	}
	return lines
}

// fitRunes returns the longest prefix of s (at least one rune) no wider than maxW.
func fitRunes(face font.Face, s string, maxW fixed.Int26_6) string {
	r := []rune(s)
	n := 1
	for n < len(r) && font.MeasureString(face, string(r[:n+1])) <= maxW {
		n++
	}
	return string(r[:n])
}

// ellipsize shortens s with a trailing "…" until it fits maxW.
func ellipsize(face font.Face, s string, maxW fixed.Int26_6) string {
	if font.MeasureString(face, s) <= maxW {
		return s
	}
	r := []rune(s)
	for n := len(r) - 1; n > 0; n-- {
		cand := strings.TrimRight(string(r[:n]), " ,.;:—-") + "…"
		if font.MeasureString(face, cand) <= maxW {
			return cand
		}
	}
	return "…"
}

// cardSize is the window size in device pixels for a body of height bodyH.
func cardSize(bodyH, scale float64) image.Point {
	return image.Pt(px(cardWidth+2*shadowPad, scale), px(bodyH+2*shadowPad, scale))
}

// hitZone classifies window pixel (x, y) of a card with body height bodyH.
func hitZone(x, y int, bodyH, scale float64) int {
	lx, ly := (float64(x)+0.5)/scale-shadowPad, (float64(y)+0.5)/scale-shadowPad
	if lx < 0 || ly < 0 || lx >= cardWidth || ly >= bodyH {
		return hitNone
	}
	if math.Hypot(lx-closeCX, ly-closeCY) <= closeRadius {
		return hitClose
	}
	return hitBody
}

// renderCard draws card c as a window image: transparent margin, soft shadow,
// rounded body, kind tile, text and a close button. hot is the hovered zone.
func renderCard(c Card, th Theme, scale float64, hot int) (*image.RGBA, cardLayout, error) {
	fs, err := newFaces(scale)
	if err != nil {
		return nil, cardLayout{}, err
	}
	l := layoutCard(c, fs, scale)
	img := image.NewRGBA(image.Rectangle{Max: cardSize(l.bodyH, scale)})
	s := func(v float64) float64 { return v * scale }
	x0, y0 := s(shadowPad), s(shadowPad)
	x1, y1 := x0+s(cardWidth), y0+s(l.bodyH)

	drawShadow(img, x0, y0+s(5), x1, y1+s(5), s(cardRadius), s(14), th.Shadow)
	border := th.Border
	if hot != hitNone {
		border = mix(th.Bg, th.Accent, 0.55)
	}
	fillRoundRectF(img, x0, y0, x1, y1, s(cardRadius), border)
	fillRoundRectF(img, x0+s(1), y0+s(1), x1-s(1), y1-s(1), s(cardRadius-1), th.Bg)

	drawTile(img, c, th, x0+s(cardInset), y0+s(cardInset), scale)

	text := func(f font.Face, str string, col color.NRGBA, x, base float64) {
		d := font.Drawer{Dst: img, Src: image.NewUniform(col), Face: f,
			Dot: fixed.Point26_6{X: toFixed(x), Y: toFixed(base)}}
		d.DrawString(str)
	}
	text(fs.title, l.title, th.Text, x0+s(textLeft), y0+s(titleBase))
	if l.time != "" {
		w := float64(font.MeasureString(fs.small, l.time)) / 64
		text(fs.small, l.time, th.Muted, x0+s(timeRight)-w, y0+s(titleBase))
	}
	refCol := th.Accent
	if c.Kind == KindGroup {
		refCol = th.Muted
	}
	text(fs.small, l.ref, refCol, x0+s(textLeft), y0+s(refBase))
	for i, line := range l.lines {
		text(fs.body, line, th.Text, x0+s(textLeft), y0+s(snippetBase+float64(i)*lineStep))
	}

	cx, cy := x0+s(closeCX), y0+s(closeCY)
	cross := th.Muted
	if hot == hitClose {
		fillCircleF(img, cx, cy, s(closeRadius), th.Surface)
		cross = th.Text
	}
	a := s(4.5)
	strokeLineF(img, cx-a, cy-a, cx+a, cy+a, s(1.6), cross)
	strokeLineF(img, cx-a, cy+a, cx+a, cy-a, s(1.6), cross)
	return img, l, nil
}

// drawTile paints the kind icon: a soft tinted square with a glyph.
func drawTile(img *image.RGBA, c Card, th Theme, x, y, scale float64) {
	s := func(v float64) float64 { return v * scale }
	tint := th.Accent
	if c.Kind == KindClosed {
		tint = th.Success
	}
	fillRoundRectF(img, x, y, x+s(tileSize), y+s(tileSize), s(10), mix(th.Bg, tint, 0.16))
	cx, cy := x+s(tileSize/2), y+s(tileSize/2)
	switch c.Kind {
	case KindIssue:
		strokeCircleF(img, cx, cy, s(8), s(2), tint)
		fillCircleF(img, cx, cy, s(2.2), tint)
	case KindComment:
		fillRoundRectF(img, cx-s(9.5), cy-s(8), cx+s(9.5), cy+s(5.5), s(4), tint)
		strokePolyF(img, []float64{cx - s(5), cy + s(4), cx - s(7), cy + s(9), cx - s(1), cy + s(4.5)}, s(2.4), tint)
		for _, dx := range []float64{-4.5, 0, 4.5} {
			fillCircleF(img, cx+s(dx), cy-s(1.3), s(1.4), th.Bg)
		}
	case KindClosed:
		fillCircleF(img, cx, cy, s(10), tint)
		strokePolyF(img, []float64{cx - s(4.6), cy + s(0.2), cx - s(1.4), cy + s(3.4), cx + s(4.8), cy - s(3.4)}, s(2.2), th.Bg)
	case KindGroup:
		for _, dy := range []float64{-5, 0, 5} {
			strokeLineF(img, cx-s(7), cy+s(dy), cx+s(7), cy+s(dy), s(2), tint)
		}
	}
}
