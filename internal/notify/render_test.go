package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font"
)

func testFaces(t *testing.T, scale float64) faces {
	t.Helper()
	fs, err := newFaces(scale)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestWrapTextCyrillic(t *testing.T) {
	fs := testFaces(t, 1.5)
	maxW := toFixed(200 * 1.5)
	short := "Краш при загрузке"
	if got := wrapText(fs.body, short, maxW, 2); len(got) != 1 || got[0] != short {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("Всплывающее уведомление приложения ", 6)
	got := wrapText(fs.body, long, maxW, 2)
	if len(got) != 2 || !strings.HasSuffix(got[1], "…") || strings.HasSuffix(got[0], "…") {
		t.Fatalf("long: %q", got)
	}
	for _, l := range got {
		if w := font.MeasureString(fs.body, l); w > maxW {
			t.Errorf("line %q width %v > %v", l, w, maxW)
		}
	}
	if !strings.HasPrefix(long, got[0]) {
		t.Errorf("first line not a prefix: %q", got[0])
	}
	// One unbreakable word wider than the line splits by runes.
	word := strings.Repeat("Щ", 60)
	got = wrapText(fs.body, word, maxW, 2)
	if len(got) != 2 || !strings.HasPrefix(word, got[0]) || !strings.HasSuffix(got[1], "…") {
		t.Fatalf("word: %q", got)
	}
	if got := wrapText(fs.body, "  ", maxW, 2); len(got) != 0 {
		t.Fatalf("blank: %q", got)
	}
	if got := wrapText(fs.body, long, maxW, 1); len(got) != 1 || !strings.HasSuffix(got[0], "…") {
		t.Fatalf("one line: %q", got)
	}
}

func TestEllipsize(t *testing.T) {
	fs := testFaces(t, 1)
	if got := ellipsize(fs.title, "Новый комментарий", toFixed(1000)); got != "Новый комментарий" {
		t.Fatalf("fits: %q", got)
	}
	got := ellipsize(fs.title, "Новый комментарий", toFixed(60))
	if !strings.HasSuffix(got, "…") || font.MeasureString(fs.title, got) > toFixed(60) || !strings.HasPrefix(got, "Нов") {
		t.Fatalf("clipped: %q", got)
	}
	if got := ellipsize(fs.title, "Ж", toFixed(1)); got != "…" {
		t.Fatalf("nothing fits: %q", got)
	}
}

func TestLayoutHeights(t *testing.T) {
	fs := testFaces(t, 1)
	now := time.Date(2026, 9, 28, 9, 5, 0, 0, time.Local)
	one := layoutCard(Card{Kind: KindIssue, Title: "Новый issue", Ref: "o/r#1", Text: "коротко", Time: now}, fs, 1)
	two := layoutCard(Card{Kind: KindComment, Title: "Новый комментарий", Ref: "o/r#2",
		Text: strings.Repeat("длинный текст комментария ", 10), Time: now}, fs, 1)
	group := layoutCard(Card{Kind: KindGroup, Title: "+3 ещё", Text: "3 новых issue"}, fs, 1)
	if one.time != "09:05" || len(one.lines) != 1 || len(two.lines) != 2 || len(group.lines) != 0 {
		t.Fatalf("layout: %+v %+v %+v", one, two, group)
	}
	if two.bodyH-one.bodyH != lineStep || group.bodyH != refBase+5+cardInset || one.bodyH <= group.bodyH {
		t.Fatalf("heights %v %v %v", one.bodyH, two.bodyH, group.bodyH)
	}
	if group.ref != "3 новых issue" {
		t.Fatalf("group summary on the ref line: %q", group.ref)
	}
}

func TestHitZone(t *testing.T) {
	for _, scale := range []float64{1, 1.25, 1.5, 2} {
		at := func(lx, ly float64) int {
			return hitZone(px(shadowPad+lx, scale), px(shadowPad+ly, scale), 86, scale)
		}
		if got := at(closeCX, closeCY); got != hitClose {
			t.Errorf("scale %v close centre: %d", scale, got)
		}
		if got := at(100, 50); got != hitBody {
			t.Errorf("scale %v body: %d", scale, got)
		}
		if got := at(-5, 40); got != hitNone {
			t.Errorf("scale %v shadow left: %d", scale, got)
		}
		if got := at(100, 90); got != hitNone {
			t.Errorf("scale %v below body: %d", scale, got)
		}
	}
}

func TestRenderCard(t *testing.T) {
	c := Card{Kind: KindClosed, Title: "Issue закрыт", Ref: "o/r#3", Text: "готово", Time: time.Now()}
	for _, scale := range []float64{1, 1.5} {
		img, l, err := renderCard(c, DarkTheme(), scale, hitClose)
		if err != nil {
			t.Fatal(err)
		}
		if img.Rect.Size() != cardSize(l.bodyH, scale) {
			t.Fatalf("size %v, want %v", img.Rect.Size(), cardSize(l.bodyH, scale))
		}
		if a := img.RGBAAt(0, 0).A; a != 0 {
			t.Errorf("corner not transparent: %d", a)
		}
		mid := img.RGBAAt(px(shadowPad+cardWidth/2, scale), px(shadowPad+l.bodyH-3, scale))
		if mid.A != 0xff {
			t.Errorf("body not opaque: %+v", mid)
		}
		for i := 0; i < len(img.Pix); i += 4 { // premultiplied: no channel above alpha
			if a := img.Pix[i+3]; img.Pix[i] > a || img.Pix[i+1] > a || img.Pix[i+2] > a {
				t.Fatalf("pixel %d not premultiplied: %v", i/4, img.Pix[i:i+4])
			}
		}
	}
}

func TestWriteSnapshots(t *testing.T) {
	dir := t.TempDir()
	files, err := WriteSnapshots(dir, 1)
	if err != nil || len(files) != 6 {
		t.Fatalf("%v %v", files, err)
	}
	for _, f := range files {
		if st, err := os.Stat(f); err != nil || st.Size() == 0 || filepath.Dir(f) != dir {
			t.Errorf("%s: %v", f, err)
		}
	}
}
