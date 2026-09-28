package notify

import (
	"image"
	"image/color"
	"math"
)

// Anti-aliased shapes for the popup card: each shape is a signed distance
// (negative inside) turned into pixel coverage, composited source-over onto
// premultiplied RGBA (image.RGBA), which UpdateLayeredWindow takes as is.

// blendPx composites straight-alpha c at coverage cov over pixel (x, y).
func blendPx(img *image.RGBA, x, y int, c color.NRGBA, cov float64) {
	if cov <= 0 || !(image.Point{X: x, Y: y}).In(img.Rect) {
		return
	}
	a := float64(c.A) / 255 * min(cov, 1)
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	inv := 1 - a
	p[0] = uint8(math.Round(float64(c.R)*a + float64(p[0])*inv))
	p[1] = uint8(math.Round(float64(c.G)*a + float64(p[1])*inv))
	p[2] = uint8(math.Round(float64(c.B)*a + float64(p[2])*inv))
	p[3] = uint8(math.Round(255*a + float64(p[3])*inv))
}

// fillSDF paints c wherever the signed distance sd is below zero, with a
// one-pixel anti-aliased edge, over the box [x0,x1)×[y0,y1).
func fillSDF(img *image.RGBA, x0, y0, x1, y1 float64, c color.NRGBA, sd func(px, py float64) float64) {
	b := image.Rect(int(math.Floor(x0))-1, int(math.Floor(y0))-1, int(math.Ceil(x1))+1, int(math.Ceil(y1))+1).Intersect(img.Rect)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			blendPx(img, x, y, c, 0.5-sd(float64(x)+0.5, float64(y)+0.5))
		}
	}
}

// sdRoundRect is the signed distance from (px, py) to a rounded rectangle.
func sdRoundRect(px, py, x0, y0, x1, y1, r float64) float64 {
	cx, cy := (x0+x1)/2, (y0+y1)/2
	hx, hy := (x1-x0)/2-r, (y1-y0)/2-r
	qx, qy := math.Abs(px-cx)-hx, math.Abs(py-cy)-hy
	outside := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	return outside + math.Min(math.Max(qx, qy), 0) - r
}

func fillRoundRectF(img *image.RGBA, x0, y0, x1, y1, r float64, c color.NRGBA) {
	fillSDF(img, x0, y0, x1, y1, c, func(px, py float64) float64 { return sdRoundRect(px, py, x0, y0, x1, y1, r) })
}

func fillCircleF(img *image.RGBA, cx, cy, r float64, c color.NRGBA) {
	fillSDF(img, cx-r, cy-r, cx+r, cy+r, c, func(px, py float64) float64 { return math.Hypot(px-cx, py-cy) - r })
}

// strokeCircleF draws a ring of width w centred on radius r.
func strokeCircleF(img *image.RGBA, cx, cy, r, w float64, c color.NRGBA) {
	o := r + w/2
	fillSDF(img, cx-o, cy-o, cx+o, cy+o, c, func(px, py float64) float64 { return math.Abs(math.Hypot(px-cx, py-cy)-r) - w/2 })
}

// strokeLineF draws a round-capped segment of width w.
func strokeLineF(img *image.RGBA, ax, ay, bx, by, w float64, c color.NRGBA) {
	h := w / 2
	fillSDF(img, math.Min(ax, bx)-h, math.Min(ay, by)-h, math.Max(ax, bx)+h, math.Max(ay, by)+h, c,
		func(px, py float64) float64 { return segDist(px, py, ax, ay, bx, by) - h })
}

// strokePolyF draws connected round-capped segments; joints are painted once.
func strokePolyF(img *image.RGBA, pts []float64, w float64, c color.NRGBA) {
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for i := 0; i+1 < len(pts); i += 2 {
		x0, y0 = math.Min(x0, pts[i]), math.Min(y0, pts[i+1])
		x1, y1 = math.Max(x1, pts[i]), math.Max(y1, pts[i+1])
	}
	h := w / 2
	fillSDF(img, x0-h, y0-h, x1+h, y1+h, c, func(px, py float64) float64 {
		d := math.Inf(1)
		for i := 0; i+3 < len(pts); i += 2 {
			d = math.Min(d, segDist(px, py, pts[i], pts[i+1], pts[i+2], pts[i+3]))
		}
		return d - h
	})
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := 0.0
	if l := dx*dx + dy*dy; l > 0 {
		t = math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/l))
	}
	return math.Hypot(px-ax-t*dx, py-ay-t*dy)
}

// drawShadow paints a soft drop shadow of a rounded rectangle: its coverage
// mask blurred by three box passes (≈ gaussian) and tinted black at opacity.
func drawShadow(img *image.RGBA, x0, y0, x1, y1, r, blur float64, opacity uint8) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	mask := make([]float64, w*h)
	for y := range h {
		for x := range w {
			mask[y*w+x] = min(max(0.5-sdRoundRect(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1, r), 0), 1)
		}
	}
	rad := max(int(math.Round(blur/3)), 1)
	tmp := make([]float64, w*h)
	for range 3 {
		boxBlur(mask, tmp, w, h, rad, 1, w) // rows
		boxBlur(tmp, mask, h, w, rad, w, 1) // columns
	}
	black := color.NRGBA{A: opacity}
	for y := range h {
		for x := range w {
			blendPx(img, x+img.Rect.Min.X, y+img.Rect.Min.Y, black, mask[y*w+x])
		}
	}
}

// boxBlur averages src over a window of 2*rad+1 along lines of length n; step
// is the stride along a line, lineStep between lines (zero outside the image).
func boxBlur(src, dst []float64, n, lines, rad, step, lineStep int) {
	norm := 1 / float64(2*rad+1)
	for l := range lines {
		base := l * lineStep
		sum := 0.0
		for i := 0; i < rad && i < n; i++ {
			sum += src[base+i*step]
		}
		for i := range n {
			if j := i + rad; j < n {
				sum += src[base+j*step]
			}
			if j := i - rad - 1; j >= 0 {
				sum -= src[base+j*step]
			}
			dst[base+i*step] = sum * norm
		}
	}
}

// mix returns c at alpha a (0..1) composited over opaque bg.
func mix(bg, c color.NRGBA, a float64) color.NRGBA {
	f := func(b, v uint8) uint8 { return uint8(math.Round(float64(b)*(1-a) + float64(v)*a)) }
	return color.NRGBA{R: f(bg.R, c.R), G: f(bg.G, c.G), B: f(bg.B, c.B), A: 0xff}
}
