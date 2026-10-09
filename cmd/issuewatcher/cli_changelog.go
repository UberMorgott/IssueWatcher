package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// changelog: `changelog list|check <project>` · `changelog set <project> --version V (--file F | -) [--dry-run]`
// · `changelog delete <project> --version V [--dry-run]` (the MCP list_mod_changelogs /
// set_mod_changelog / delete_mod_changelog).
func (c *cli) changelog(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	cmd := "changelog " + sub
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch sub {
	case "list", "check":
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		if sub == "check" {
			return c.c.CheckChangelogs(c.ctx, id)
		}
		return c.c.Changelogs(c.ctx, id)
	case "set", "delete":
		version := fs.String("version", "", "the mod version")
		dry := fs.Bool("dry-run", false, "plan only, nothing is sent")
		var file *string
		if sub == "set" {
			file = fs.String("file", "", "text file: one changelog entry per non-empty line (leading \"- \" dropped)")
		}
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		stdin := sub == "set" && len(pos) == 2 && pos[1] == "-"
		if stdin {
			pos = pos[:1]
		}
		if strings.TrimSpace(*version) == "" {
			return nil, usagef("%s: want --version", cmd)
		}
		id, err := c.project(cmd, pos)
		if err != nil {
			return nil, err
		}
		if sub == "delete" {
			return c.c.DeleteChangelog(c.ctx, id, *version, *dry)
		}
		if stdin == (*file != "") {
			return nil, usagef("%s: give the lines with --file FILE or - (stdin)", cmd)
		}
		var b []byte
		if stdin {
			b, err = io.ReadAll(io.LimitReader(c.stdin, 256<<10))
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: read: %w", cmd, err)
		}
		lines := provider.ChangelogLines(string(trimBOM(b)))
		if len(lines) == 0 {
			return nil, usagef("%s: no changelog lines (use changelog delete to remove a version)", cmd)
		}
		return c.c.SetChangelog(c.ctx, id, *version, lines, *dry)
	}
	return nil, usagef("changelog: want list, check, set or delete")
}
