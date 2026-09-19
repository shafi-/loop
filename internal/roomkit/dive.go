// The room's deep-dive launcher: turns a chat.DiveSpec into a
// one-agent-stage pipeline file and runs it on the host's existing
// supervisor — the same machinery as /run (gates ask in the chat,
// /approve answers them, usage attributes by run id). One coordinator
// per hosted room; both hosting surfaces (CLI and daemon) wire it, so
// the semantics cannot drift.
package roomkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/executor/cline"
	"gopkg.in/yaml.v3"
)

// DiveAliasPrefix marks supervisor aliases that are deep dives. The
// coordinator claims exactly these in the host's handler callbacks;
// ordinary /run aliases pass through untouched.
const DiveAliasPrefix = "dive-"

// diveStageID is the generated pipeline's only stage; the read-back
// knows where the report lives.
const diveStageID = "dive"

// DiveCoordinator owns one room's dives: the alias registry (alias →
// agent/topic), the generated pipeline files, and the report landing.
type DiveCoordinator struct {
	roomDir string // .loop/rooms/<room> — dives/ lives here
	runsDir string // .loop/runs — report read-back
	tr      *chat.Transcript
	// start spawns the run on the host's supervisor (alias + file).
	start func(alias, file string) error
	// register teaches the host's alias resolution the dive file, so
	// /approve <alias>, /run <alias> --resume <id>, and the runs view
	// all resolve (review amendment A2).
	register func(alias, file string)
	// notice renders to the live surface (terminal UI); the transcript
	// line is the durable record either way. nil = transcript only.
	notice func(format string, args ...any)

	mu    sync.Mutex
	metas map[string]diveMeta // supervisor alias → what is running
}

type diveMeta struct {
	Agent string
	Topic string
}

// AttachDives wires deep-dive launching into a hosted room. When the
// executor is unavailable the room cannot dive: StartDive stays nil
// (so agents never offer and /deepdive explains why) and the returned
// coordinator is nil — hosts then have nothing to delegate. The
// availability check runs once here, at assembly; never per offer.
func AttachDives(r *chat.Room, tr *chat.Transcript, roomDir, runsDir string, start func(alias, file string) error, register func(alias, file string), notice func(format string, args ...any)) *DiveCoordinator {
	if start == nil {
		r.DiveUnavailableReason = "this surface has no run session — attach with loop chat or the daemon"
		return nil
	}
	if problems := cline.New().Check(); len(problems) > 0 {
		r.DiveUnavailableReason = "the agent executor is not ready (" + problems[0].Error() + ") — run: loop setup"
		return nil
	}
	c := &DiveCoordinator{
		roomDir: roomDir, runsDir: runsDir, tr: tr,
		start: start, register: register, notice: notice,
		metas: map[string]diveMeta{},
	}
	r.StartDive = c.Start
	return c
}

// Start implements chat.Room.StartDive: build the pipeline file, teach
// the host's resolution, spawn. Errors surface as the room's ↻ line
// (the caller in chat handles that) — never a failed turn.
func (c *DiveCoordinator) Start(_ context.Context, spec chat.DiveSpec) error {
	alias := DiveAliasPrefix + spec.Agent
	file, err := c.writePipeline(spec)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.metas[alias] = diveMeta{Agent: spec.Agent, Topic: spec.Topic}
	c.mu.Unlock()
	if c.register != nil {
		c.register(alias, file)
	}
	return c.start(alias, file)
}

// Handles reports whether a supervisor driver belongs to the
// coordinator (host handlers delegate dive drivers here).
func (c *DiveCoordinator) Handles(alias string) bool {
	return strings.HasPrefix(alias, DiveAliasPrefix)
}

// OnEnded lands the dive report in the transcript when a claimed run
// completes. Hosts call it from their OnEnded for drivers they pass
// through (alias, run id, terminal kind); non-dive aliases are ignored.
// Paused/failed runs keep the host's generic lines — the report only
// exists on completion.
func (c *DiveCoordinator) OnEnded(alias, runID, kind string) {
	if kind != "completed" || !c.Handles(alias) {
		return
	}
	c.mu.Lock()
	meta, ok := c.metas[alias]
	delete(c.metas, alias)
	c.mu.Unlock()
	if !ok {
		return
	}
	report, ok := engine.ReadStageOutput(c.runsDir, runID, diveStageID)
	if !ok {
		c.appendLine(fmt.Sprintf("@%s (deep dive): the run completed but left no report — see .loop/runs/%s/context.json", meta.Agent, runID))
		return
	}
	c.appendLine(fmt.Sprintf("@%s (deep dive): %s — run %s", meta.Agent, clipDiveReport(report), runID))
}

// maxDiveReportLine caps the transcript line; the run record keeps the
// full text (context.json), the line points at it via the run id.
const maxDiveReportLine = 2 << 10

func clipDiveReport(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxDiveReportLine {
		return s
	}
	return s[:maxDiveReportLine] + "… (full report in the run's context.json)"
}

func (c *DiveCoordinator) appendLine(text string) {
	_ = c.tr.Append("system", text)
	if c.notice != nil {
		c.notice("%s", text)
	}
}

// writePipeline renders the dive pipeline and stores it under the
// room's dives/ directory — auditable and re-runnable next to the
// transcript.
func (c *DiveCoordinator) writePipeline(spec chat.DiveSpec) (string, error) {
	dir := filepath.Join(c.roomDir, "dives")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("dive dir: %w", err)
	}
	data, err := yaml.Marshal(buildDivePipeline(spec))
	if err != nil {
		return "", fmt.Errorf("dive pipeline: %w", err)
	}
	name := fmt.Sprintf("dive-%s-%s.yaml", spec.Agent, time.Now().UTC().Format("20060102-150405"))
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return "", fmt.Errorf("dive pipeline: %w", err)
	}
	return file, nil
}

// divePipelineYAML is the generated pipeline's exact shape — a local
// struct with explicit tags, NOT config.Pipeline (Stage has a custom
// unmarshaler and no marshaler; round-tripping it is not guaranteed).
type divePipelineYAML struct {
	Name     string        `yaml:"name"`
	Personas []divePersona `yaml:"personas"`
	Stages   []diveStage   `yaml:"stages"`
}

type divePersona struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role,omitempty"`
	System string `yaml:"system,omitempty"`
}

type diveStage struct {
	ID            string   `yaml:"id"`
	Type          string   `yaml:"type"`
	Persona       string   `yaml:"persona"`
	Executor      string   `yaml:"executor"`
	Tools         []string `yaml:"tools,omitempty"`
	Approval      string   `yaml:"approval"`
	MaxIterations int      `yaml:"max_iterations"`
	Input         string   `yaml:"input"`
	Output        string   `yaml:"output"`
}

// diveSystemPrompt closes the reply with a report contract.
const diveInputTail = `Work in this workspace. Verify before asserting: read the
relevant code (search broadly first), and run checks when they settle a
question — commands ask the room before they run, so prefer a few
decisive ones. Then answer from your role's perspective: findings with
file paths, what you verified and how, what remains uncertain. Keep it
tight — this lands in the room as your reply.`

// buildDivePipeline assembles the spec into the generated pipeline.
// project_notes is filtered from the grants: it is a room-native tool
// the cline host does not know, and the digest already rides the
// persona's system prompt.
func buildDivePipeline(spec chat.DiveSpec) divePipelineYAML {
	var tools []string
	for _, t := range spec.Tools {
		if t == "project_notes" {
			continue
		}
		tools = append(tools, t)
	}
	role := spec.Role
	if role == "" {
		role = "colleague"
	}
	return divePipelineYAML{
		Name: "deep-dive-" + spec.Agent,
		Personas: []divePersona{{
			Name:   spec.Agent,
			Role:   role,
			System: spec.System,
		}},
		Stages: []diveStage{{
			ID:            diveStageID,
			Type:          "agent",
			Persona:       spec.Agent,
			Executor:      "cline",
			Tools:         tools,
			Approval:      "ask",
			MaxIterations: 12,
			Input: fmt.Sprintf(`Deep-dive request from the room.

Topic: %s

Original question:
%s

Room context (recent conversation):
%s

%s`, spec.Topic, spec.Question, spec.ContextTail, diveInputTail),
			Output: diveStageID,
		}},
	}
}
