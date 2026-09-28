package notify

import (
	"image"
	"math"
)

// Edge is the screen edge the taskbar sits on.
type Edge int

// Taskbar edges.
const (
	EdgeBottom Edge = iota
	EdgeTop
	EdgeLeft
	EdgeRight
)

const (
	edgeGap  = 12 // logical px between the work-area edge and the nearest card
	stackGap = 8  // logical px between cards
)

// TaskbarEdge infers the taskbar edge from the part of the monitor the work
// area leaves out; an auto-hidden taskbar leaves nothing out → bottom.
func TaskbarEdge(monitor, work image.Rectangle) Edge {
	switch {
	case work.Max.Y < monitor.Max.Y:
		return EdgeBottom
	case work.Min.Y > monitor.Min.Y:
		return EdgeTop
	case work.Min.X > monitor.Min.X:
		return EdgeLeft
	case work.Max.X < monitor.Max.X:
		return EdgeRight
	}
	return EdgeBottom
}

// px converts logical pixels to device pixels at scale (DPI / 96).
func px(v, scale float64) int { return int(math.Round(v * scale)) }

// placeStack returns the top-left window position of each card window. sizes
// are window sizes in device pixels, each including the shadow margin
// (shadowPad) on every side; index 0 is nearest the anchor corner. The stack
// sits in the bottom-right corner of the work area, growing upwards; with the
// taskbar on top (tray in the top-right) it hangs from the top-right corner.
func placeStack(monitor, work image.Rectangle, sizes []image.Point, scale float64) []image.Point {
	m, edge, gap := px(shadowPad, scale), px(edgeGap, scale), px(stackGap, scale)
	top := TaskbarEdge(monitor, work) == EdgeTop
	out := make([]image.Point, len(sizes))
	right := work.Max.X - edge + m // window right edge (body right + margin)
	if top {
		y := work.Min.Y + edge // next body top
		for i, s := range sizes {
			out[i] = image.Pt(right-s.X, y-m)
			y += s.Y - 2*m + gap
		}
		return out
	}
	y := work.Max.Y - edge // next body bottom
	for i, s := range sizes {
		body := s.Y - 2*m
		out[i] = image.Pt(right-s.X, y-body-m)
		y -= body + gap
	}
	return out
}
