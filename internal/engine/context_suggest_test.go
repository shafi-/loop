package engine

import (
	"strings"
	"testing"
)

func TestUnknownStageReferenceSuggestsNearest(t *testing.T) {
	c := NewContext(nil)
	// Without known stage ids — a run started before the load-time gate
	// existed — the message stays generic.
	if _, err := c.Interpolate("${stages.architect_signoff.intent}"); err == nil || !strings.Contains(err.Error(), "(stage has not run)") {
		t.Fatalf("default message changed: %v", err)
	}

	c.SetKnownStages([]string{"requirements", "architect-signoff"})
	if _, err := c.Interpolate("${stages.architect_signoff.intent}"); err == nil || !strings.Contains(err.Error(), `did you mean "architect-signoff"`) {
		t.Fatalf("want nearest-id suggestion, got: %v", err)
	}
	if _, err := c.Interpolate("${stages.entirely-unrelated.output}"); err == nil || strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), "no stage with this id") {
		t.Fatalf("want bare no-such-id message, got: %v", err)
	}
	// A known id that simply has not run yet keeps the not-run wording.
	if _, err := c.Interpolate("${stages.architect-signoff.intent}"); err == nil || !strings.Contains(err.Error(), "(stage has not run)") {
		t.Fatalf("known-id-not-run wording changed: %v", err)
	}
}
