// Package web embeds the browser UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS serves index.html at the root.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
