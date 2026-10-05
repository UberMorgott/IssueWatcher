// Package steamugc creates Steam Workshop items and writes their store page
// in every language through ISteamUGC (steam_api64.dll) bound to the running,
// signed-in Steam client — no login, the owner's own Steam session, as the
// game's official workshop tools do. steamcmd's workshop_build_item VDF has
// one title/description only; ISteamUGC::SetItemUpdateLanguage writes each
// language's title and description (one submit per language).
//
// The API runs in a short-lived helper process (the app's own binary with
// HelperArg) so the app never binds to a game's app id itself.
package steamugc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrBadPage: the page sources are missing or invalid (nothing was sent).
var ErrBadPage = errors.New("steam: bad workshop page")

// Languages are Steam's API language codes
// (https://partner.steamgames.com/doc/store/localization/languages).
var Languages = []string{"arabic", "bulgarian", "schinese", "tchinese", "czech", "danish", "dutch", "english", "finnish", "french",
	"german", "greek", "hungarian", "indonesian", "italian", "japanese", "koreana", "malay", "norwegian", "polish", "portuguese",
	"brazilian", "romanian", "russian", "spanish", "latam", "swedish", "thai", "turkish", "ukrainian", "vietnamese"}

// Visibilities maps the names to ERemoteStoragePublishedFileVisibility.
var Visibilities = map[string]int{"public": 0, "friends": 1, "private": 2, "unlisted": 3}

const (
	maxTitle       = 128  // k_cchPublishedDocumentTitleMax - 1
	maxDescription = 8000 // k_cchPublishedDocumentDescriptionMax
	maxPreview     = 1 << 20
	maxTags        = 20
)

// PageSpec says where the page comes from, relative to the mod's folder.
type PageSpec struct {
	// LocaleDir holds description.<language>.txt (BBCode) and optional
	// title.<language>.txt; default workshop/locale.
	LocaleDir string `json:"localeDir,omitempty"`
	// Title is every language's title unless title.<language>.txt exists.
	Title string `json:"title,omitempty"`
	// Preview is the preview image (≤ 1 MB jpg/png/gif); default
	// workshop/image/steam_preview.jpg or image/steam_preview.jpg when present,
	// "-" = leave the preview as it is.
	Preview string `json:"preview,omitempty"`
	// Tags replace the item's tags (item-global); nil = unchanged.
	Tags []string `json:"tags,omitempty"`
	// Visibility: public | friends | private | unlisted; "" = unchanged.
	Visibility string `json:"visibility,omitempty"`
}

// Text is one language's title and description.
type Text struct {
	Language    string `json:"language"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Page is a resolved page: english first.
type Page struct {
	Texts      []Text   `json:"texts"`
	Preview    string   `json:"preview,omitempty"` // absolute path
	Tags       []string `json:"tags,omitempty"`    // nil = unchanged
	Visibility int      `json:"visibility"`        // -1 = unchanged
	Sources    []string `json:"sources,omitempty"` // the files read (relative)
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadPage, fmt.Sprintf(format, a...))
}

func readText(path string) (string, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the mod's own page sources
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !utf8.Valid(b) {
		return "", false, bad("%s is not UTF-8", filepath.Base(path))
	}
	s := strings.TrimPrefix(string(b), "\uFEFF")
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")), true, nil
}

// inside resolves rel against folder and refuses paths that leave it.
func inside(folder, rel string) (string, error) {
	if filepath.IsAbs(rel) || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", bad("%s must be a path inside the mod folder", rel)
	}
	return filepath.Join(folder, filepath.FromSlash(rel)), nil
}

// LoadPage reads the page of the mod in folder: one Text per
// description.<language>.txt (english required).
func LoadPage(folder string, spec PageSpec) (Page, error) {
	if folder == "" || !filepath.IsAbs(folder) {
		return Page{}, bad("the project has no mapped folder")
	}
	dirRel := spec.LocaleDir
	if dirRel == "" {
		dirRel = "workshop/locale"
	}
	dir, err := inside(folder, dirRel)
	if err != nil {
		return Page{}, err
	}
	p := Page{Visibility: -1}
	order := append([]string{"english"}, slices.DeleteFunc(slices.Clone(Languages), func(l string) bool { return l == "english" })...)
	for _, lang := range order {
		desc, ok, err := readText(filepath.Join(dir, "description."+lang+".txt"))
		if err != nil {
			return Page{}, err
		}
		if !ok {
			if lang == "english" {
				return Page{}, bad("%s/description.english.txt is missing", dirRel)
			}
			continue
		}
		title, hasTitle, err := readText(filepath.Join(dir, "title."+lang+".txt"))
		if err != nil {
			return Page{}, err
		}
		if !hasTitle {
			title = strings.TrimSpace(spec.Title)
		}
		switch {
		case title == "":
			return Page{}, bad("no title for %s: give title or %s/title.%s.txt", lang, dirRel, lang)
		case utf8.RuneCountInString(title) > maxTitle:
			return Page{}, bad("%s title is longer than %d characters", lang, maxTitle)
		case desc == "":
			return Page{}, bad("%s/description.%s.txt is empty", dirRel, lang)
		case utf8.RuneCountInString(desc) > maxDescription:
			return Page{}, bad("%s/description.%s.txt is longer than %d characters", dirRel, lang, maxDescription)
		}
		p.Texts = append(p.Texts, Text{Language: lang, Title: title, Description: desc})
		p.Sources = append(p.Sources, dirRel+"/description."+lang+".txt")
		if hasTitle {
			p.Sources = append(p.Sources, dirRel+"/title."+lang+".txt")
		}
	}
	if err := p.preview(folder, spec.Preview); err != nil {
		return Page{}, err
	}
	if spec.Tags != nil {
		if len(spec.Tags) > maxTags {
			return Page{}, bad("at most %d tags", maxTags)
		}
		p.Tags = []string{}
		for _, t := range spec.Tags {
			if t = strings.TrimSpace(t); t != "" && !slices.Contains(p.Tags, t) {
				if len(t) > 255 || strings.ContainsAny(t, ",\x00") {
					return Page{}, bad("tag %q", t)
				}
				p.Tags = append(p.Tags, t)
			}
		}
	}
	if spec.Visibility != "" {
		v, ok := Visibilities[spec.Visibility]
		if !ok {
			return Page{}, bad("visibility: public, friends, private or unlisted")
		}
		p.Visibility = v
	}
	return p, nil
}

var previewDefaults = []string{"workshop/image/steam_preview.jpg", "image/steam_preview.jpg", "workshop/image/steam_preview.png", "image/steam_preview.png"}

func (p *Page) preview(folder, rel string) error {
	if rel == "-" {
		return nil
	}
	cands := previewDefaults
	if rel != "" {
		cands = []string{rel}
	}
	for _, c := range cands {
		path, err := inside(folder, c)
		if err != nil {
			return err
		}
		st, err := os.Stat(path)
		if err != nil {
			if rel != "" {
				return bad("preview %s not found", rel)
			}
			continue
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".jpg", ".jpeg", ".png", ".gif":
		default:
			return bad("preview %s: a jpg, png or gif", c)
		}
		if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxPreview {
			return bad("preview %s must be a file of at most 1 MB (it is %d bytes)", c, st.Size())
		}
		p.Preview = path
		p.Sources = append(p.Sources, c)
		return nil
	}
	return nil
}
