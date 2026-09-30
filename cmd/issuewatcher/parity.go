package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/parity"
	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// nativeForParity builds a native provider for the parity check; work is a
// scratch data dir (browser profile, no stored sessions), account the stored
// source account. Filled per platform as the native engines land.
var nativeForParity = map[string]func(work, account string) (provider.Provider, func(), error){}

// runParity is the dev command `issuewatcher parity --db COPY --platform ID
// [--project EXT]`: a native read of every stored project compared with the
// rows of a database COPY (opened read-only; nothing written, no events).
func runParity(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("parity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "snapshot copy of issuewatcher.db (never the live file)")
	platform := fs.String("platform", "", "nexus | curseforge | factorio")
	only := fs.String("project", "", "one project external id")
	if err := fs.Parse(args); err != nil || *db == "" || *platform == "" {
		_, _ = fmt.Fprintln(stderr, "usage: parity --db COPY --platform ID [--project EXT]")
		return exitUsage
	}
	build, ok := nativeForParity[*platform]
	if !ok {
		_, _ = fmt.Fprintln(stderr, "issuewatcher: parity: no native engine for", *platform)
		return exitUsage
	}
	if err := parityRun(*db, *platform, *only, build, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "issuewatcher: parity:", err)
		return exitAPI
	}
	return exitOK
}

func parityRun(db, platform, only string, build func(string, string) (provider.Provider, func(), error), stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	snap, err := parity.Open(db)
	if err != nil {
		return err
	}
	defer func() { _ = snap.Close() }()
	account, err := snap.Account(ctx, platform)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "iw-parity-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	prov, closeFn, err := build(filepath.Join(work, "data"), account)
	if err != nil {
		return err
	}
	defer closeFn()
	start := time.Now()
	reps, err := parity.Run(ctx, snap, platform, prov, only)
	out := struct {
		Platform string          `json:"platform"`
		Account  string          `json:"account"`
		Seconds  float64         `json:"seconds"`
		Reports  []parity.Report `json:"reports"`
		Error    string          `json:"error,omitempty"`
	}{Platform: platform, Account: account, Seconds: time.Since(start).Seconds(), Reports: reps}
	if err != nil {
		out.Error = err.Error()
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if eerr := enc.Encode(out); eerr != nil {
		return eerr
	}
	if err != nil {
		return err
	}
	for _, r := range reps {
		if len(r.Diffs) > 0 {
			return errors.New("differences found")
		}
	}
	return nil
}
