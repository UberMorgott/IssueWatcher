package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

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
		appID := fs.Uint("app-id", 0, "start the API as this app (default: the project's Steam target app)")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		if *appID > 1<<31 {
			return nil, usagef("workshop status: bad --app-id")
		}
		var id int64
		if len(pos) > 0 {
			if id, err = c.project(cmd, pos); err != nil {
				return nil, err
			}
		}
		if id == 0 && *appID == 0 {
			return nil, usagef("workshop status: want a project with a Steam target or --app-id")
		}
		return c.c.SteamStatus(c.ctx, id, uint32(*appID))
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

// comment: `comment --steam-item ID (--body-file FILE | -) [--dry-run]` posts a new
// top-level comment on a Steam Workshop item (not a reply).
func (c *cli) comment(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	item := fs.String("steam-item", "", "Workshop item id (publishedfileid)")
	file := fs.String("body-file", "", "file with the comment body")
	dry := fs.Bool("dry-run", false, "resolve the endpoint and creator only; post nothing")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	stdin := len(pos) == 1 && pos[0] == "-"
	if (len(pos) > 0 && !stdin) || stdin == (*file != "") {
		return nil, usagef("comment: give the body with --body-file FILE or - (stdin)")
	}
	if _, err := strconv.ParseUint(*item, 10, 64); err != nil {
		return nil, usagef("comment: want --steam-item <publishedfileid>")
	}
	var b []byte
	if stdin {
		b, err = io.ReadAll(io.LimitReader(c.stdin, 1<<20))
	} else {
		b, err = os.ReadFile(*file)
	}
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return c.c.SteamComment(c.ctx, *item, string(trimBOM(b)), *dry)
}
