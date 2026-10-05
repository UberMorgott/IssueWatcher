package main

import (
	"encoding/json"
	"flag"

	"github.com/UberMorgott/issuewatcher/internal/control"
)

// workshop: `workshop status` (the running Steam client) and `workshop item|create|page <project> …` (Steam Workshop items of a code project).
func (c *cli) workshop(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	cmd := "workshop " + sub
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch sub {
	case "status":
		appID := fs.Uint("app-id", 0, "start the API as this app (default 480)")
		if _, err := flags(fs, args[1:]); err != nil {
			return nil, err
		}
		if *appID > 1<<31 {
			return nil, usagef("workshop status: bad --app-id")
		}
		return c.c.SteamStatus(c.ctx, uint32(*appID))
	case "item":
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		return c.c.WorkshopItem(c.ctx, id)
	case "create":
		appID := fs.Uint("app-id", 0, "the game's Steam app id")
		dry := fs.Bool("dry-run", false, "plan only")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		if *appID == 0 || *appID > 1<<31 {
			return nil, usagef("workshop create: want --app-id")
		}
		return c.c.CreateWorkshopItem(c.ctx, id, uint32(*appID), *dry)
	case "page":
		var p control.WorkshopPage
		fs.StringVar(&p.Item, "item", "", "Workshop item id (default: the project's)")
		appID := fs.Uint("app-id", 0, "the game's app id (default: the profile target's)")
		fs.StringVar(&p.Title, "title", "", "title of every language without title.<language>.txt (default: the project name)")
		fs.StringVar(&p.LocaleDir, "locale-dir", "", "folder with description.<language>.txt (default workshop/locale)")
		fs.StringVar(&p.Preview, "preview", "", "preview image in the repo (default workshop/image/steam_preview.jpg or image/steam_preview.jpg; - = keep)")
		tags := fs.String("tags", "", "comma-separated tags (replace; omit to keep)")
		fs.StringVar(&p.Visibility, "visibility", "", "public, friends, private or unlisted (omit to keep)")
		fs.StringVar(&p.ChangeNote, "change-note", "", "change note")
		fs.BoolVar(&p.DryRun, "dry-run", false, "plan only")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		if *appID > 1<<31 {
			return nil, usagef("workshop page: bad --app-id")
		}
		p.AppID = uint32(*appID)
		if *tags != "" {
			p.Tags = splitList(*tags)
		}
		return c.c.SetWorkshopPage(c.ctx, id, p)
	}
	return nil, usagef("workshop: want status, item, create or page")
}
