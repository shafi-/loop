package generator

import (
	_ "embed"

	"github.com/nerddevsltd/loop/internal/examples"
)

//go:embed contract.md
var pipelineContract string

// The worked example is the same embedded file `loop init` scaffolds —
// one source of truth (see internal/examples).
var examplePipeline = examples.FeaturePipeline
