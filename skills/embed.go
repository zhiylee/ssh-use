// Package skills bundles the agent instructions with the CLI so installation
// works without a checkout or network access and tracks the CLI version.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed ssh-use
var bundled embed.FS

// Files returns the installable ssh-use skill, rooted at its SKILL.md.
func Files() fs.FS {
	skill, err := fs.Sub(bundled, "ssh-use")
	if err != nil {
		panic(err) // The directory is guaranteed by go:embed at build time.
	}
	return skill
}
