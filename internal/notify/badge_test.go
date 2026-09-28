package notify

import (
	"bytes"
	"image/png"
	"testing"
)

func TestBadgeLabel(t *testing.T) {
	cases := map[int]string{-1: "", 0: "", 1: "1", 42: "42", 99: "99", 100: "99+", 5000: "99+"}
	for in, want := range cases {
		if got := BadgeLabel(in); got != want {
			t.Errorf("BadgeLabel(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTrayIconDrawsBadge(t *testing.T) {
	plain, err := TrayIcon(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 42, 150} {
		data, err := TrayIcon(n)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("count %d: invalid PNG: %v", n, err)
		}
		if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
			t.Fatalf("count %d: size %v", n, b)
		}
		if bytes.Equal(data, plain) {
			t.Fatalf("count %d: icon identical to no-badge icon", n)
		}
		// Right edge of the pill at mid-height: badge red, never a digit pixel.
		r, g, b, _ := img.At(iconSize-2, iconSize-7).RGBA()
		if r>>8 != uint32(colorBadge.R) || g>>8 != uint32(colorBadge.G) || b>>8 != uint32(colorBadge.B) {
			t.Fatalf("count %d: expected badge colour near bottom-right, got %d,%d,%d", n, r>>8, g>>8, b>>8)
		}
	}
}
