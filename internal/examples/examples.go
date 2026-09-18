// Package examples embeds the canonical example documents. Both the CLI
// scaffolder and the NL→pipeline generator use the same source so the
// few-shot example can never drift from what `loop init` writes.
package examples

import _ "embed"

//go:embed feature-pipeline.yaml
var FeaturePipeline string

// The starter kit: the shipped dev team (global personas), one room,
// and two pipelines for regular development — everything a fresh
// install needs to be useful in a minute, embedded so it travels with
// the binary.

//go:embed starter/personas/architect.yaml
var StarterArchitect string

//go:embed starter/personas/engineer.yaml
var StarterEngineer string

//go:embed starter/personas/reviewer.yaml
var StarterReviewer string

//go:embed starter/personas/product-owner.yaml
var StarterProductOwner string

//go:embed starter/personas/cfo.yaml
var StarterCFO string

//go:embed starter/personas/end-user.yaml
var StarterEndUser string

//go:embed starter/dev-room.yaml
var DevRoom string

//go:embed starter/feature-room.yaml
var FeatureRoom string

//go:embed starter/implement-pipeline.yaml
var ImplementPipeline string

//go:embed starter/review-pipeline.yaml
var ReviewPipeline string

// BuiltinPersonas returns the shipped persona library contents by
// name — the identities seeded into ~/.loop/personas/ so every project
// on the machine can reference them (persona: <name>). Two sides: the
// build team (architect, engineer, reviewer) and the decision side
// that shapes features (product-owner, cfo, end-user).
func BuiltinPersonas() map[string]string {
	return map[string]string{
		"architect":     StarterArchitect,
		"engineer":      StarterEngineer,
		"reviewer":      StarterReviewer,
		"product-owner": StarterProductOwner,
		"cfo":           StarterCFO,
		"end-user":      StarterEndUser,
	}
}
