package api

import (
	"embed"
	"io/fs"
)

// The web bundle ships embedded so the binary is a single artifact —
// nothing for the user to chmod or place on disk.

//go:embed web/*
var webFS embed.FS

// staticFS exposes the embedded files rooted at "web/" so HTTP requests for
// "/" serve "web/index.html" without leaking the prefix.
func staticFS() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embed: " + err.Error())
	}
	return sub
}
