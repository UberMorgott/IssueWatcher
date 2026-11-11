package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/UberMorgott/issuewatcher/internal/control"
)

// page: `page get <project>` · `page set <project> [--name N] [--summary S] [--version V]
// [--description-file FILE | -] [--dry-run]` (the MCP get_mod_page / set_mod_page).
func (c *cli) page(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	cmd := "page " + sub
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch sub {
	case "get":
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		return c.c.ModPage(c.ctx, id)
	case "set":
		name := fs.String("name", "", "new mod name")
		summary := fs.String("summary", "", "new summary")
		version := fs.String("version", "", "new mod version")
		file := fs.String("description-file", "", "file with the whole new description (BBCode)")
		dry := fs.Bool("dry-run", false, "plan only, nothing is sent")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		stdin := len(pos) == 2 && pos[1] == "-"
		if stdin {
			pos = pos[:1]
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		if stdin && *file != "" {
			return nil, usagef("%s: give the description with --description-file FILE or - (stdin), not both", cmd)
		}
		edit := control.ModPageEdit{DryRun: *dry}
		// Only flags given on the command line are sent: an omitted field keeps the page's value.
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "name":
				edit.Name = name
			case "summary":
				edit.Summary = summary
			case "version":
				edit.Version = version
			}
		})
		if stdin || *file != "" {
			var b []byte
			if stdin {
				b, err = io.ReadAll(io.LimitReader(c.stdin, 1<<20))
			} else {
				b, err = os.ReadFile(*file)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: read: %w", cmd, err)
			}
			d := string(trimBOM(b))
			edit.Description = &d
		}
		if edit.Name == nil && edit.Summary == nil && edit.Version == nil && edit.Description == nil {
			return nil, usagef("%s: nothing to change (want --name, --summary, --version, --description-file or -)", cmd)
		}
		return c.c.SaveModPage(c.ctx, id, edit)
	}
	return nil, usagef("page: want get or set")
}
