// Package issuewatcher carries the built frontend bundle inside the binary.
//
// An embed directive only sees its own directory and descendants, so this file
// lives at the module root next to frontend/.
package issuewatcher

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend/dist
var dist embed.FS

// Assets returns the frontend bundle rooted at frontend/dist.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "frontend/dist")
	if err != nil {
		panic(err) // impossible: compile-time embedded directory
	}
	return sub
}
