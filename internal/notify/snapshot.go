package notify

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"time"
)

// WriteSnapshots renders sample popups to PNG files in dir without creating a
// window (dev switch IW_POPUP_SNAPSHOT): dark and light, each as a single
// card, a closed card and a stack with a "+N ещё" group card, composited on a
// plain desktop-like backdrop at the given scale. It returns the file paths.
func WriteSnapshots(dir string, scale float64) ([]string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("notify: snapshot dir: %w", err)
	}
	now := time.Date(2026, 9, 28, 14, 32, 0, 0, time.Local)
	comment := Card{Kind: KindComment, Title: "Новый комментарий", Ref: "UberMorgott/IssueWatcher#42", Time: now, ItemID: "42",
		Text: "octocat: Проверил на Windows 11 — всплывающее окно появляется, но после клика дашборд открывается в новой вкладке вместо уже открытой."}
	issue := Card{Kind: KindIssue, Title: "Новый issue", Ref: "UberMorgott/PhoenixPoint#7", Time: now, ItemID: "7",
		Text: "alice: Краш при загрузке сохранения"}
	closed := Card{Kind: KindClosed, Title: "Issue закрыт", Ref: "UberMorgott/IssueWatcher#39", Time: now, ItemID: "39",
		Text: "Уведомления Windows подавляются в режиме «Не беспокоить»"}

	st := newStack(DefaultAutoHide)
	st.now = func() time.Time { return now }
	for _, c := range []Card{closed, issue, closed, issue, closed, issue, comment} {
		st.push(c)
	}
	var stackCards []Card
	for _, e := range st.visible() {
		stackCards = append(stackCards, e.card)
	}

	var out []string
	for _, t := range []struct {
		name     string
		theme    Theme
		backdrop color.NRGBA
	}{
		{"dark", DarkTheme(), rgb(0x2e, 0x36, 0x42)},
		{"light", LightTheme(), rgb(0xd5, 0xdc, 0xe5)},
	} {
		for _, shot := range []struct {
			name  string
			cards []Card
		}{
			{"single", []Card{comment}},
			{"closed", []Card{closed}},
			{"stack", stackCards},
		} {
			img, err := composeStack(shot.cards, t.theme, t.backdrop, scale)
			if err != nil {
				return out, err
			}
			path := filepath.Join(dir, "popup-"+t.name+"-"+shot.name+".png")
			if err := writePNG(path, img); err != nil {
				return out, err
			}
			out = append(out, path)
		}
	}
	return out, nil
}

// composeStack places cards (anchor-first) as placeStack would in the
// bottom-right corner of a backdrop just large enough to hold them.
func composeStack(cards []Card, th Theme, backdrop color.NRGBA, scale float64) (*image.RGBA, error) {
	imgs := make([]*image.RGBA, len(cards))
	sizes := make([]image.Point, len(cards))
	height := px(2*edgeGap, scale)
	for i, c := range cards {
		img, l, err := renderCard(c, th, scale, hitNone)
		if err != nil {
			return nil, err
		}
		imgs[i], sizes[i] = img, img.Rect.Size()
		height += px(l.bodyH+stackGap, scale)
	}
	area := image.Rect(0, 0, px(cardWidth+2*edgeGap+40, scale), height+px(20, scale))
	dst := image.NewRGBA(area)
	draw.Draw(dst, area, image.NewUniform(backdrop), image.Point{}, draw.Src)
	for i, p := range placeStack(area, area, sizes, scale) {
		draw.Draw(dst, imgs[i].Rect.Add(p), imgs[i], image.Point{}, draw.Over)
	}
	return dst, nil
}

func writePNG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return fmt.Errorf("notify: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("notify: write snapshot: %w", err)
	}
	return nil
}
