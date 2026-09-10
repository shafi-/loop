package examples

import (
	_ "embed"
	"testing"

	"github.com/nerddevsltd/loop/internal/config"
)

// The example doubles as a few-shot prompt for the generator. If it ever
// stops validating, every generated pipeline would be taught a broken
// contract — so pin it.
func TestFeaturePipelineAlwaysValidates(t *testing.T) {
	if _, err := config.ParsePipeline([]byte(FeaturePipeline)); err != nil {
		t.Fatalf("embedded example pipeline no longer validates: %v", err)
	}
}
