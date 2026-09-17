package generator

import (
	_ "embed"

	"github.com/shafi-/loop/internal/examples"
)

//go:embed contract.md
var pipelineContract string

// The worked example is the same embedded file `loop init` scaffolds —
// one source of truth (see internal/examples).
var examplePipeline = examples.FeaturePipeline

//go:embed roomcontract.md
var roomContract string

//go:embed roomexample.yaml
var exampleRoom string
