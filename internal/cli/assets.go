package cli

import (
	_ "embed"
)

//go:embed assets/room.yaml
var assetRoom string

//go:embed assets/workspace-readme.md
var assetReadme string
