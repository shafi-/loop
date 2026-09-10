// Package examples embeds the canonical example documents. Both the CLI
// scaffolder and the NL→pipeline generator use the same source so the
// few-shot example can never drift from what `loop init` writes.
package examples

import _ "embed"

//go:embed feature-pipeline.yaml
var FeaturePipeline string
