# Deep-dive escalation for room personas — architecture

Status: REVIEWED (independent review 2026-09-20: CONFIRMED-WITH-CHANGES;
amendments A1–A8 below folded in — those are binding).
Some `internal/agent` + `internal/chat` pieces are already drafted and
building (marked ✅ below); everything else is pending. Nothing is wired
into hosts yet, so no behavior has changed for users.

## 0. Review amendments (binding)

- **A1 (MAJOR) — `/approve <dive-alias>` must resolve in the local
  CLI.** The CLI decides alias-vs-answer via `resolveFile`, which only
  knows room pipelines and `.yaml` paths — `dive-architect` would be
  swallowed into the answer text (declining the gate). Fix: the
  CLI's `/approve` (and `/halt`, `/status` already fine) checks live
  supervisor aliases (`sup.Runs()`) before `resolveFile`. Daemon and
  attach-CLI paths already pass the alias through — no change.
- **A2 (MAJOR) — dive resume must resolve.** `/run dive-<agent>
  --resume <id>` cannot resolve today on either host, and the hosts
  print that hint themselves. Fix: when `StartDive` writes the dive
  YAML it registers alias→file in the session's resolvable set (CLI:
  `roomRunSession` gains a dives map consulted by `resolveFile`;
  daemon: register in `s.specs` — which also fixes the runs dashboard's
  file resolution). Resume hint lines then work verbatim.
- **A3 (MAJOR) — completion read-back gets a named owner.** Supervisor
  `Handlers` are host-constructed closures; `room.StartDive` cannot
  intercept `OnEnded`. Fix: `roomkit.DiveCoordinator` — owns the
  alias→meta registry, claims drivers whose alias starts `dive-`, and
  exposes `Start` (writes YAML, registers resolution, invokes a
  host-provided start func) plus `OnNotice/OnGate/OnEnded` that both
  hosts call from their existing handlers (the coordinator appends the
  report line itself; hosts keep their generic rendering).
- **A4 (MINOR) — busy-click:** the offer card is visible while the
  daemon room is mid-turn; a click then 409s (toast shown, command
  lost). Accepted for now — the offer persists, retry works; documented
  in the manual.
- **A5 (MINOR) — CLI rotation guard:** the local CLI attaches no
  Guard to /reset //fork (daemon-only today). Add the same runs-alive
  guard in the CLI session so a dive can't be rotated out from under.
- **A6 (MINOR) — usage label:** record executor-side tokens as
  `stage:<id>` (existing engine convention), not `agent:<stage>`.
- **A7 (MINOR) — availability gate:** one `Check()` per room assembly
  is fine (script mode spawns `node --version` — once, not per offer);
  drop the redundant `StandaloneHost()` disjunct; split the nil-hook
  error message (no run session vs executor not installed).
- **A8 (NIT, docs):** gate answers are strict `yes`/`y` — free text
  declines. State it in the manual's dive section.

## 1. Goal and user experience

Room personas get a third tier of capability, on demand:

- **Tier 0 — chat** (unchanged): direct provider call, streamed.
- **Tier 1 — light tools** (unchanged): the native Go loop
  (`replyWithTools`) — read_file/write_file/run_command/project_notes,
  8 rounds, path-guarded, in-process.
- **Tier 2 — deep dive** (new): the persona's investigation runs
  through the cline executor as a supervised child `loop run` — search,
  many files, commands with human approval — launched only by an
  explicit user action.

The flow (design decision by the user, 2026-09-20):

1. You ask `@architect` something code-specific.
2. Before replying, the architect runs one cheap structured
   decision: "would my answer be materially better after real
   investigation?" (≈300 tokens, no tools executed).
3. The architect replies from what it has; if the decision flagged a
   dive, the room appends a deterministic system line right after:

       ◈ deep-dive offer @architect — topic: session invalidation in auth/ — run: /deepdive @architect

4. The line's CTA is **cosmetic in the UI**: the web room view renders
   that marker line as an offer card with a button; clicking the button
   just sends `/deepdive @architect` through the normal say endpoint.
   Terminal users type the same command.
5. `/deepdive` builds a one-agent-stage pipeline and starts it like
   `/run`: progress as `▸` transcript lines, command approval as
   `[approval needed]` lines answered with the existing `/approve`,
   and the final report lands as an `@architect (deep dive): …`
   transcript line carrying the run id (which is how `/cost` already
   attributes pipeline spend — no new accounting).

There is **no consent detection anywhere**: no vocabulary lists, no
classifier call, no sole-offer heuristics, no expiry logic. Launching
is always an explicit command, so the engine never parses free text and
cannot false-fire. Ignored offers cost nothing and never expire (they
are suggestions, not pending state).

## 2. Layer-by-layer changes

### 2.1 `internal/agent` — dive detection ✅ drafted, builds

- `Decision` gains `DeepDive bool` / `Topic string` (`dive_topic`).
- `speakSchema` extended with `deep_dive` + `dive_topic` (required, for
  OpenAI strict mode) — observers get dive detection for free inside
  the speak call they already make. Prompt guidance added: true only
  when the digest/notes/conversation likely don't cover it; topic must
  be specific enough to start from.
- New `DecideDive(ctx, conversation) (Decision, error)` for **tagged**
  agents (they reply without a speak call today): one structured call,
  `diveSchema` `{deep_dive, dive_topic, reason}`, MaxTokens 300, on the
  agent's own (metered) provider.
- New `Base(room)` — exports the composed identity preface (persona
  system + workspace brief + knowledge digest + room framing) so the
  dive run carries the *same* grounding as a normal reply.
- Known cost: one extra structured call per tagged reply by a
  tool-using persona. Accepted deliberately (deterministic offers);
  failure degrades silently to no offer with a UI notice.

### 2.2 `internal/chat` — offer lines + the /deepdive command ✅ drafted, builds

New file `dive.go`:

- `DiveSpec{Agent, Topic, Question, Tools, System, ContextTail}` —
  everything the host launcher needs, composed from room state.
- `diveOfferMarker = "◈ deep-dive offer"` — the UI contract: any
  transcript system line starting with this marker renders as an offer
  card (web) or plain text (terminal). The line embeds the exact
  command: `run: /deepdive @architect`.
- `deepdive.json` beside the transcript: last offered topic+question
  per agent, loaded lazily, written atomically (tmp+rename, the
  `usage.json` pattern). Convenience only — `/deepdive @agent` without
  a topic reuses the last offered one. No expiry: offers are
  suggestions.
- `offerDive(agent, topic, question, ui)`: record + append offer line +
  UI notice.
- `launchDive`: builds the DiveSpec (Tools from persona grants, System
  from `agent.Base`, ContextTail from the transcript window) and calls
  the injected hook. Hook errors → system line, never a failed turn.
- `/deepdive [@agent] [topic]` as a built-in `CustomCommand` — one door
  for clickers and typers, same semantics on every client (the
  CustomCommand design goal). Explicit topic beats remembered topic;
  no topic and no offer → usage error naming agents that can dive.
- `canDive(agent)` = `StartDive != nil && len(Persona.Tools) > 0` —
  the room never offers what it cannot run.

`room.go` integration (minimal):

- `Room.StartDive func(ctx, DiveSpec) error` — host-injected hook, same
  pattern as `RunUsage`/`Summarizer`. nil ⇒ no offers, `/deepdive`
  explains how to attach.
- `Say` phase 1 (tagged): before replying, `DecideDive` when `canDive`;
  after the reply, `offerDive` when flagged. Phase 3 (observers): the
  extended `DecideSpeak` result offers the same way.
- `NewRoom` registers the `deepdive` command.
- **Deleted by design** (user's CTA decision): consent detection, the
  phase-0 hook in `Say`, skip-the-consented-agent logic, `consentSchema`,
  CTA regex parsing, offer expiry/turn counting.

### 2.3 `internal/roomkit` — the launcher (shared by CLI + daemon) — PENDING

- `BuildDivePipeline(spec) []byte`: renders a one-agent-stage pipeline.
  **Hand-built via a small local struct with explicit yaml tags — not
  `yaml.Marshal(config.Pipeline)`** (Stage has custom UnmarshalYAML and
  no matching marshal; round-tripping is not guaranteed). Shape:

      name: deep-dive-<agent>
      personas:
        - name: <agent>
          role: <role>
          system: <Base as a block scalar>
      stages:
        - id: dive
          type: agent
          persona: <agent>
          executor: cline
          tools: [<persona grants minus project_notes>]
          approval: ask
          max_iterations: 12
          input: |
            Deep-dive request... topic, original question, context tail,
            "verify in this workspace, then answer from your role's
            perspective; commands ask the room first."
          output: dive_report

  `project_notes` is filtered out (room-native tool; the cline host
  would report it as unknown — and the digest already rides `system:`).
- Written to `.loop/rooms/<room>/dives/dive-<agent>-<ts>.yaml` (the
  generated pipeline is kept next to the transcript — auditable,
  re-runnable).
- `StartDive` implementation: `sup.Start(Spec{Alias: "dive-<agent>",
  File: <written file>})` on the session's existing `runctl.Supervisor`
  — a second concurrent dive by the same agent hits the supervisor's
  per-alias busy error and surfaces as the ↻ system line. Gates ask in
  the chat via the handlers that already exist. `/approve`, `/halt`,
  `/status` work on `dive-<agent>` unmodified.
- **Completion read-back**: on `OnEnded(completed)`, read the run's
  `context.json` (`outputs.dive_report`) and append
  `@<agent> (deep dive): <report trimmed> — run <run-id>`. The run id
  in the line is what `pipelineUsage`'s regex attributes — /cost shows
  the dive without new accounting. New accessor
  `engine.ReadStageOutput(runsDir, runID, stageID) (string, bool)`
  (context.json layout verified: `{"vars", "stages", "outputs"}`).
- **Availability gate**: wire `StartDive` only when the executor is
  usable (`cl := cline.New(); len(cl.Check()) == 0 || cl.StandaloneHost()`
  — cheap stats, once at assembly). Unavailable ⇒ hook stays nil ⇒ no
  offers dangle, `/deepdive` explains. (Decided: not a launch-time
  error — offers must not lie.)
- Wiring points: CLI `roomRunSession` (chat.go) and daemon `roomSession`
  (rooms.go) both already own a supervisor + transcript + runsDir; both
  set `room.StartDive` to the shared implementation.

### 2.4 `internal/executor` + cline host — riders — PENDING

- **Usage event**: the SDK emits `usage-updated` (runtime event,
  cumulative `inputTokens`/`outputTokens`). Host maps it to
  `{"type":"usage","input":I,"output":O}`. `executor.Event` gains
  `EventUsage` (+ token fields); `cline.go` maps it; `runAgentStage`
  records the delta into `d.Usage` under `agent:<stage>`. Closes the
  pre-existing pipeline metering gap (executor-side tokens were
  invisible) — same rider the pipeline side needs anyway.
- **Confinement**: custom `fileRead` executor override in
  `createBuiltinTools` (the SDK's builtin resolves absolute paths
  as-is and relative against `process.cwd()` with no guard — verified
  against the installed d.ts). Override mirrors the room loop's
  `safePath`: reject absolute, reject `..` escape, resolve against the
  task cwd, keep `maxFileSizeBytes`. Editor keeps its default
  `restrictToCwd` (relative-only confinement; absolute paths still
  pass — documented limitation, mitigated by approval=ask on writes?
  no: editor writes are auto-approved by design... **risk R3 below**).
  `executorOptions.bash.timeoutMs = 120_000` (the room loop uses 60s;
  dives may run test suites).

### 2.5 UIs — the CTA — PENDING (small)

- Terminal: nothing to do — the offer line already shows the command.
- WebUI (`internal/webui/chatview.go`): system lines become
  `Kind: "system"` messages. Add detection: a system message starting
  with `◈ deep-dive offer` renders as an offer card (agent name, topic)
  with a button whose click handler posts the line's `/deepdive @agent`
  text to the existing say endpoint (htmx, same as the composer). The
  button is cosmetic — no new endpoint, no new state.
- Daemon: nothing — the transcript *is* the broadcast; `/deepdive`
  arrives as an ordinary say text and hits the room command.

### 2.6 Docs — PENDING

- USER MANUAL (rooms section): the three tiers, the offer line and its
  marker contract, `/deepdive`, command approval during dives, executor
  availability requirement.
- `.loop/rooms/dev.yaml` + `feature.yaml` header comments: which
  personas may offer dives (tools granted) and that grants are per-room
  overrides of the library.

## 3. End-to-end walkthroughs

**A. Tagged question → offer → dive.**
`Say("@architect why do sessions leak?")` → transcript append → tagged
phase: `DecideDive` (1 structured call) → `reply` streams the answer →
decision flagged + topic → offer line appended. User clicks CTA → POST
say `/deepdive @architect` → room command → remembered topic →
`launchDive` → `StartDive` hook → roomkit writes the dive YAML, starts
supervisor alias `dive-architect` → `▶` line. Child run: agent stage →
cline executor → host (tools bound, approval ask) → `run_commands` call
→ `approval_request` → engine `agentApprover` → `d.Human.Prompt` →
`human_prompt` event → supervisor `OnGate` → `[approval needed]`
transcript line → user `/approve yes` (existing routing writes the
child's stdin) → run completes → `OnEnded` → read `outputs.dive_report`
→ `@architect (deep dive): …` line. `/cost` attributes via the run id.

**B. Observer offer.** Untagged message; `DecideSpeak` returns
speak + deep_dive; after the spontaneous reply, the same offer line.

**C. Explicit dive, no offer.** `/deepdive @engineer where is rate
limiting enforced` — direct launch, remembered offers untouched.

## 4. Concurrency & lifecycle

- `Transcript.Append` is mutex-guarded (verified, transcript.go:69-73);
  supervisor callbacks already append from their goroutines — the dive
  completion handler is the same pattern.
- Daemon room `busy` mutex guards *turns*, not runs: dives run in the
  supervisor, so a dive never blocks (or deadlocks behind) the say
  path; the room stays typeable while a dive runs. (This is why the
  in-turn synchronous executor design was rejected.)
- Rotation: `/reset`/`/fork` guards already veto while any supervised
  run is alive/waiting — dives are supervised runs, covered.
- Restart: a running dive is a daemon-owned child run; resume via
  `/run dive-<agent> --resume <run-id>` (same as pipelines). Offers
  persist in `deepdive.json`.
- Multiple simultaneous dives: allowed (distinct aliases per agent);
  same-agent re-dive while running → alias busy → ↻ line.

## 5. Risks / open gaps

- **R1 — offer latency**: DecideDive adds a call before every tagged
  tool-using reply. Mitigation if it annoys: room setting
  `dive_offers: false` to disable (not in this iteration; note for
  follow-up).
- **R2 — decision quality**: an agent that always offers is noise.
  The prompt pins "true only when the notes/conversation don't cover
  it" + specific-topic requirement; tuning is prompt-level, no
  machinery.
- **R3 — editor confinement is relative-only** (SDK `restrictToCwd`
  accepts absolute paths by design). Shell is unrestricted by design.
  Both are mitigated by `approval: ask` gating run_commands and by
  dives being explicit, user-initiated runs in the user's own
  workspace. Documented limitation, not solved here (real sandboxing
  = SubprocessSandbox, a separate effort).
- **R4 — Base size**: the composed system prompt includes the digest
  block; large repos → large prompts per dive. Accepted: one dive per
  explicit launch, and `max_tokens` is untouched.
- **R5 — report truncation**: dive reports land as one transcript
  line; cap at ~2 KiB with pointer to `context.json` (the run record
  keeps the full text).
- **R6 — webui button scope**: only the room chat view; the timeline
  view shows the raw line (fine).

## 6. Tests

- `internal/agent` (✅ pattern exists): DecideSpeak request carries the
  extended schema; DecideDive happy path + invalid JSON (mock pattern,
  agent_test.go:63).
- `internal/chat`: offer line + deepdive.json written after a flagged
  reply (recorderUI, decisionJSON mocks); `/deepdive` dispatches
  StartDive (stub hook) with remembered topic; explicit topic wins;
  non-tool agent and missing hook produce the error lines; observer
  offer path.
- `internal/roomkit`: BuildDivePipeline output parses via
  `config.ParsePipeline` (catches YAML/escaping regressions
  deterministically); `project_notes` filtered; completion read-back
  via a fake run dir.
- `internal/executor/cline`: fake host emits `usage` lines → EventUsage
  mapping (fakeHost pattern); engine metering test with a scripted
  provider already exists to extend for the `agent:` label.
- WebUI: marker-line rendering test (chatview pattern).

## 7. Implementation order (after review confirmation)

1. roomkit: BuildDivePipeline + StartDive + availability gate + wiring
   (CLI & daemon) + engine.ReadStageOutput.
2. Host riders: usage event (host → executor.Event → engine metering),
   fileRead override, bash timeout; reinstall + smoke.
3. WebUI CTA card.
4. Tests per section 6 as each lands; `go test ./...` green.
5. Docs; manual smoke: dev room offer → click → gate → report; /cost.
