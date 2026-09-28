package notify

import (
	"image"
	"testing"
)

func TestTaskbarEdge(t *testing.T) {
	mon := image.Rect(0, 0, 1920, 1080)
	for _, tc := range []struct {
		work image.Rectangle
		want Edge
	}{
		{image.Rect(0, 0, 1920, 1040), EdgeBottom},
		{image.Rect(0, 40, 1920, 1080), EdgeTop},
		{image.Rect(60, 0, 1920, 1080), EdgeLeft},
		{image.Rect(0, 0, 1860, 1080), EdgeRight},
		{mon, EdgeBottom}, // auto-hide
	} {
		if got := TaskbarEdge(mon, tc.work); got != tc.want {
			t.Errorf("work %v: %v, want %v", tc.work, got, tc.want)
		}
	}
}

// bodies strips the shadow margin off placed windows.
func bodies(pos, sizes []image.Point, scale float64) []image.Rectangle {
	m := px(shadowPad, scale)
	out := make([]image.Rectangle, len(pos))
	for i, p := range pos {
		out[i] = image.Rectangle{Min: p, Max: p.Add(sizes[i])}.Inset(m)
	}
	return out
}

func TestPlaceStack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mon, work  image.Rectangle
		scale      float64
		growsDown  bool
		wantRightX int // body right edge
	}{
		{"bottom 100%", image.Rect(0, 0, 1920, 1080), image.Rect(0, 0, 1920, 1040), 1, false, 1920 - 12},
		{"bottom 150%", image.Rect(0, 0, 2880, 1620), image.Rect(0, 0, 2880, 1560), 1.5, false, 2880 - 18},
		{"top 125%", image.Rect(0, 0, 2400, 1350), image.Rect(0, 50, 2400, 1350), 1.25, true, 2400 - 15},
		{"left 100%", image.Rect(0, 0, 1920, 1080), image.Rect(62, 0, 1920, 1080), 1, false, 1920 - 12},
		{"right 200%", image.Rect(0, 0, 3840, 2160), image.Rect(0, 0, 3720, 2160), 2, false, 3720 - 24},
		{"secondary origin", image.Rect(-1920, 0, 0, 1080), image.Rect(-1920, 0, 0, 1040), 1, false, -12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sizes := []image.Point{cardSize(104, tc.scale), cardSize(86, tc.scale), cardSize(64, tc.scale)}
			pos := placeStack(tc.mon, tc.work, sizes, tc.scale)
			b := bodies(pos, sizes, tc.scale)
			edge, gap := px(edgeGap, tc.scale), px(stackGap, tc.scale)
			for i, r := range b {
				if r.Max.X != tc.wantRightX {
					t.Errorf("card %d right %d, want %d", i, r.Max.X, tc.wantRightX)
				}
				if !r.In(tc.work) {
					t.Errorf("card %d %v outside work area %v", i, r, tc.work)
				}
			}
			if tc.growsDown {
				if b[0].Min.Y != tc.work.Min.Y+edge {
					t.Errorf("first top %d, want %d", b[0].Min.Y, tc.work.Min.Y+edge)
				}
				for i := 1; i < len(b); i++ {
					if b[i].Min.Y != b[i-1].Max.Y+gap {
						t.Errorf("card %d top %d, want %d", i, b[i].Min.Y, b[i-1].Max.Y+gap)
					}
				}
				return
			}
			if b[0].Max.Y != tc.work.Max.Y-edge {
				t.Errorf("first bottom %d, want %d", b[0].Max.Y, tc.work.Max.Y-edge)
			}
			for i := 1; i < len(b); i++ {
				if b[i].Max.Y != b[i-1].Min.Y-gap {
					t.Errorf("card %d bottom %d, want %d", i, b[i].Max.Y, b[i-1].Min.Y-gap)
				}
			}
		})
	}
}
