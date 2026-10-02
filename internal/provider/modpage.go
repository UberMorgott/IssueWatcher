package provider

import (
	"context"
	"errors"
)

// Editing a mod page's general fields from the app (Phase 7; Nexus first): a
// provider with Capabilities.EditPage implements PageEditor.

// ErrBadPageEdit: an edit the platform would refuse (checked before sending).
var ErrBadPageEdit = errors.New("bad mod page edit")

// ErrCannotEdit: the signed-in account may not edit this mod page.
var ErrCannotEdit = errors.New("this account cannot edit the mod page")

// PageEditor reads and saves a mod page's name, summary, description and version.
type PageEditor interface {
	// ModPage reads the page as the site's editor loads it.
	ModPage(ctx context.Context, project Project) (ModPage, error)
	// SaveModPage saves edit (nil fields unchanged); every other field of the
	// page is resent exactly as loaded. DryRun builds the request and sends nothing.
	SaveModPage(ctx context.Context, project Project, edit ModPageEdit) (ModPageSave, error)
}

// ModPage is the editable view of a mod page.
type ModPage struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`     // plain text, line breaks as \n
	Description string   `json:"description"` // BBCode
	Version     string   `json:"version"`
	Category    string   `json:"category,omitempty"`
	Author      string   `json:"author,omitempty"`
	Tags        []string `json:"tags"`
	URL         string   `json:"url"`
	// MaxSummary is the longest summary the platform accepts (characters).
	MaxSummary int `json:"maxSummary,omitempty"`
}

// ModPageEdit is a save request; nil = keep the current value.
type ModPageEdit struct {
	Name        *string `json:"name,omitempty"`
	Summary     *string `json:"summary,omitempty"`
	Description *string `json:"description,omitempty"`
	Version     *string `json:"version,omitempty"`
	DryRun      bool    `json:"dryRun,omitempty"`
}

// ModPageSave is the outcome of a save (or its plan when DryRun).
type ModPageSave struct {
	DryRun bool `json:"dryRun,omitempty"`
	// Changed are the fields that differ from the loaded page (name, summary, description, version).
	Changed []string    `json:"changed"`
	Request PublishStep `json:"request"`
	Saved   bool        `json:"saved,omitempty"`
}
