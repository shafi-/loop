package runctl

import "strings"

// Translator turns the child's stderr lines into host callbacks. It is a
// pure state machine: the same lines in, the same callbacks out, which is
// what makes the shapes below testable.
//
// Line shapes come from run.go and terminalHuman:
//
//	run <id> starting: <name> (N stages)      progress
//	→ <stage> (<type>)                        progress
//	⤷ <router> routed to <target>             progress
//	── your input needed ──────…               gate banner (prompt follows)
//	(/pause · /quit · /exit …)                 gate hint: prompt complete
//	✓ run <id> complete (N steps)             ended: completed
//	⏸ run <id> paused at stage "<id>"         ended: paused
//	✗ run <id> failed at stage "<id>": …      ended: failed
type Translator struct {
	gateOpen bool
	gateLive bool // a gate prompt was delivered, not yet resolved
	prompt   []string

	Notice func(text string)   // progress one-liner
	Gate   func(prompt string) // full question; /approve answers it
	Ended  func(kind, headline string)
}

// Feed processes one stderr line (newline-trimmed by the reader).
func (t *Translator) Feed(line string) {
	line = strings.TrimRight(line, "\r")
	if t.gateOpen {
		// The hint marker ends the question; the stray "> " that follows
		// never arrives as a line of its own.
		if strings.HasPrefix(line, "(/pause") {
			t.gateOpen = false
			t.gateLive = true
			if t.Gate != nil {
				t.Gate(strings.Join(t.prompt, "\n"))
			}
			t.prompt = nil
			return
		}
		t.prompt = append(t.prompt, line)
		return
	}
	if strings.TrimSpace(line) == "" || strings.TrimSpace(line) == ">" {
		return // filler around prompts and banners
	}
	// The child's terminal prompt ends with a dangling "> " that glues
	// to the next stderr line; strip the marker (prompt content itself
	// is only accumulated inside gate mode, untouched by this).
	line = strings.TrimPrefix(line, "> ")
	switch {
	case strings.HasPrefix(line, "── your input needed"):
		t.gateOpen = true
		t.prompt = nil
		return
	case strings.HasPrefix(line, "✓ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("completed", line)
		}
		return
	case strings.HasPrefix(line, "⏸ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("paused", line)
		}
		return
	case strings.HasPrefix(line, "✗ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("failed", line)
		}
		return
	case strings.HasPrefix(line, "  resume with:"):
		return // the child's shell-form hint; the host prints its own
	}
	if t.gateLive {
		t.gateLive = false // work resumed after the answer
	}
	if t.Notice != nil {
		t.Notice(line)
	}
}

// Waiting reports whether a gate question is outstanding.
func (t *Translator) Waiting() bool { return t.gateLive }

func (t *Translator) resolve() { t.gateLive = false }
