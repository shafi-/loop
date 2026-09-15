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

// The example deliberately demonstrates explicit model blocks (env-driven
// configs omit them), so guard that its agent stages keep them.
func TestFeaturePipelineAgentStagesHaveModels(t *testing.T) {
	p, err := config.ParsePipeline([]byte(FeaturePipeline))
	if err != nil {
		t.Fatal(err)
	}
	personas := map[string]*config.ModelConfig{}
	for i := range p.Personas {
		personas[p.Personas[i].Name] = p.Personas[i].Model
	}
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type != config.StageAgent {
			continue
		}
		if s.Agent.Model != nil {
			continue
		}
		if m := personas[s.Agent.Persona]; m != nil {
			continue
		}
		t.Errorf("agent stage %q (persona %q) lost its model block — the example teaches explicit models", s.ID, s.Agent.Persona)
	}
}
