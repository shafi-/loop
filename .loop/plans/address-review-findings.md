# Plan: Address the three-persona review findings

**Status:** v3 — **consensus reached**: all three personas APPROVE (PSE 95% · PO 93% · EU 91%); their non-blocking suggestions are folded in
**Date:** 2026-09-26 (v1 same day; v2 after persona review)
**Inputs:** 3 review rounds × 3 personas (principal engineer, product owner, end user). Findings were empirically verified in review rounds 2–3; the plan's code anchors were re-verified against the working tree by all three plan reviewers (see Appendix A).

## The one-sentence theme

Three independent reviewers converged on the same verdict — *strong core, dishonest edges* — so this plan is sequenced as: **safety net → trust → honesty → product**, with no new features until the first three phases land.

## Guiding principles

1. **Honesty is the product.** Every silent failure (swallowed var, false success, invisible 401, zero-cost ledger) becomes a visible one.
2. **No new features until Phases 0–2 land.** The don't-build list (Phase 4) is binding for this plan.
3. **Every fix lands with its test and its doc edit in the same PR.** Doc drift was itself a finding class.
4. **Small PRs**, each independently shippable and revertible.
5. **Validation and runtime must agree.** Where this plan changes what `validate` checks, the matching runtime path changes in the same PR (see 2.16/D4) — a validator that blesses what `/run` rejects is a new lie.

---

## Phase 0 — Safety net

### 0.1 CI workflow
- **Problem:** 87 test files exist; nothing runs them (`.github/workflows/` has only `release.yml`, `site.yml`).
- **Fix:** new `.github/workflows/test.yml`: on push/PR → `go vet ./...`, `gofmt -l` (empty output required), `go test -race ./...`.
- **Hazard (PSE):** `-race` may trip on the not-yet-fixed transcript race (1.3). Run `-race ./...` locally first; if red, land 1.3 in the same batch as 0.1 rather than skipping tests.
- **Test:** the workflow is the test; verify it catches a deliberately broken assertion before merging.
- **Size:** S. **First PR, everything else depends on it.**

---

## Phase 1 — Trust & integrity (the P0s)

### 1.1 Web UI origin hardening
- **Problem:** mutating POSTs (`webui.go:159+`: `/runs`, `/runs/{id}/answer|halt|resume`, `/rooms/{name}/say|run|approve`, plus new/personas CRUD — 16 POST routes today) accept cross-site requests — live-verified 200 + submitted run with `Origin: http://evil.example`. No Host check → DNS rebinding exposes reads. Separately, `ui.js:64` renders model-controlled `act.what` via `innerHTML` (the only dynamic sink; the other `innerHTML` sites are `""` clears).
- **Fix (three layers, one PR):**
  1. **Host allowlist middleware — on the webui TCP mux only.** Accept `Host` in {`127.0.0.1`, `localhost`, `[::1]`} any port. The unix-socket daemon mux is **excluded** (not browser-reachable, no rebinding surface, third parties may dial the socket with an arbitrary Host); `daemon` is likewise **not** in the webui allowlist — it exists only on the socket side (`client.go` base URL, `webui.go:144` proxy director). Kills DNS rebinding; binding stays loopback.
  2. **Origin/Referer check on POST only:** if the header is present, its host must be in the same allowlist, else `403`. Absent header (curl/scripts) passes; browsers always send `Origin` on cross-site POSTs (and on same-origin POSTs, which is why htmx flows keep working). Belt: reject `Sec-Fetch-Site: cross-site` when present.
  3. **`ui.js:64`:** build the activity pill with `createElement`/`textContent`. While the file is open, also fix the selector interpolation at `ui.js:449` (same class, not exploitable today).
- **Files:** `internal/webui/webui.go`, `internal/webui/assets/ui.js`.
- **Tests:** table-driven `httptest`, driven from an explicit route-count assertion so a new POST route can't land unwired: every POST route × {cross-site Origin → 403, `Origin: null` → 403, evil Host → 403, missing Host → 403, no Origin → 200, same-origin → 200}; GET with evil Host → 403; `[::1]:port` accepted (bracket parsing via `net.SplitHostPort`); Sec-Fetch-Site belt tested independently of Origin (the belt must catch, not mask).
- **Doc:** manual §4 dashboard note — "origin-checked, still local-only, never expose the port."
- **Size:** M. **PO launch-blocker #1.**

### 1.2 Run-ID collisions
- **Problem:** `runlog.go:27` — timestamp + 16 bits (`UnixNano&0xffff`; measured: 200k same-second samples → 16,383 distinct IDs); `runlog.go:40` `MkdirAll` + `:46` append → same-second collision merges two runs' audits.
- **Fix:** `newRunID` = `20060102-150405-` + 8 bytes `crypto/rand` hex. `CreateRunLog` switches to exclusive create (`os.Mkdir`) with 3 retries on fresh IDs, then a real error.
- **ID ownership (PSE, must):** `CreateRunLog` takes the ID as input and IDs are generated at three call sites — `internal/cli/run.go:163`, `internal/daemon/server_runs.go:150` (echoed in the submit response and rendered as the run link by `webui.go:395`), `internal/runctl/supervisor.go:156`. The retry must not silently diverge from what the caller displayed: `CreateRunLog` returns the canonical (possibly regenerated) ID and **all three call sites use the returned value**. With 8 random bytes the retry is near-dead code anyway — that's the point.
- **Tests:** 10k IDs in one simulated second all distinct; pre-created dir → retry produces a different ID; **caller-visible ID equals the created directory name**; exhaustion → error.
- **Compat:** run IDs are opaque to consumers; changelog notes the format change. Also update `NewRunID`'s doc comment in the same PR — its "display the id before handing control" rationale goes stale once callers use the returned ID (PSE).
- **Size:** S.

### 1.3 Transcript data race
- **Problem:** `chat/room.go:317` and `:389` read `len(r.Transcript.Messages)` unlocked while supervisor goroutines append (reproduced under `-race`); the third unlocked read is `lastUser` (`room.go:555-562`), called from `dive.go:152`, reading `t.Messages` directly.
- **Fix:** add locked accessors `Transcript.Len()` and `Transcript.LastUser()`; use them at `room.go:317`, `:389`, and `dive.go:152`; sweep for any remaining direct `Transcript.Messages` reads.
- **Tests:** concurrent Append + Len + LastUser under `-race` in CI (Phase 0 makes this permanent).
- **Size:** S.

### 1.4 Supply chain: bun + installer
- **Problem:** `executor/cline/bun.go` fetches `releases/latest` — unpinned, unverified. `install.sh` (~line 41) fetches SHA256SUMS with `|| true` → verification silently skipped when unreachable; **and** when sums are fetched but the asset line is absent, verification also falls through.
- **Fix:** pin a bun version constant + embedded per-platform sha256; download → verify → extract; mismatch = hard fail. `install.sh`: **both** failure modes (sums unreachable, asset not listed) become fatal with a clear message; no silent skip.
- **Tests:** digest verification unit test (tampered input fails); `shellcheck install.sh`; documented bun-bump procedure in a maintainer note.
- **Size:** M.

### 1.5 Path-confinement honesty *(added in v2 — promoted from PSE's "missing" list)*
- **Problem:** `safePath` (`agent/tools.go:224`) is lexical only — a symlink inside the workspace or `.loop/plans/` redirects `read_file`/`write_file` anywhere on disk (escape reproduced in review round 2). The comment at `tools.go:146-149` ("the room cannot mutate code no matter what the model decides") overclaims exactly what the machinery doesn't enforce.
- **Fix:** resolve with `filepath.EvalSymlinks` (target must remain under the workspace/plans root) before the prefix check; rewrite the comment to state the real invariant: "writes are plan-only; the shell is filtered-by-convention and human-gated" — matching the honest disclaimer already at `tools.go:260`. Implementation notes (PSE): `EvalSymlinks` fails on not-yet-existing write targets — evaluate the nearest existing ancestor, then join the remainder; the containment check must be separator-safe (`filepath.Rel` or trailing-separator compare, not naive `HasPrefix`) to avoid the `/workspace-evil` prefix bug.
- **Tests:** symlink inside `.loop/plans/` → target outside workspace → write refused; plain path → allowed.
- **Size:** S. Security-adjacent, so it ships with Phase 1 rather than being deferred.

**Phase 1 exit criteria:** all P0s closed with the tests above; re-running the PSE's curl probe and the symlink-escape reproduction both fail closed.

---

## Phase 2 — Honesty & the broken journey

*PR order within this phase follows the end user's seat: the review.yaml-in-CI adoption path unblocks by the second PR, not the fifth.*

### PR A — CLI honesty (keeper-use-case first)
- **2.3 Unknown `--var` fails `loop run` (decision D5, amended):** `run.go:140` assigns typos silently; `roomrun.go:192` warns. **Ruling: plain `loop run` hard-fails (exit 1) with the declared-vars list; rooms keep warn** (interactive, stderr is on screen). Rationale (EU): CI does not read stderr warnings — a typo'd `--var pr_num=123` in a scheduled workflow must not produce a silent wrong review. Silent is today's behavior, so failing breaks no working flow; it only breaks typos, which should break. Test: typo → exit 1 + message naming declared vars; declared var → proceeds; room `/run` → still warns.
- **2.4 `loop digest` honesty:** `digest.go:63` prints the `files:` line unconditionally. **Fix:** no eligible content → "nothing to digest yet" and no `files:` line; errors → nonzero exit. Tests: fresh repo → honest message; injected error → exit ≠ 0.
- **2.5 Group-command exit codes:** `loop executor <typo>` prints help and exits 0. **Fix:** group commands get a `RunE` that errors with "unknown command" + suggestions → exit 1. `loop executor doctor` genuinely doesn't exist — root `loop doctor` covers executor health (`executor.go:93-111`); docs fixed in 2.13.
- **2.6 Documented exit codes** *(pulled forward from 3.4 — EU must)*: document the exit-code contract (0 ok · 1 run failed · 2 usage · gate-rejection distinguishable from crash from provider-401) in manual §5. review.yaml-in-CI needs this the day the journey replays, not in Phase 3. **PR A must pin the taxonomy before 2.6b ships, since the recipe acts on the codes** (EU): decide which failures are 1 vs 2 — a var typo is arguably "usage" — and keep doc + implementations on one convention.
- **2.6b CI recipe for the keeper flow** *(EU should, adopted)*: `examples/ci/review.yml` — a real workflow snippet (checkout → install loop → `loop run .loop/pipelines/review.yaml` with the key from a secret → act on the documented exit codes → surface the cost line) plus the same recipe in manual §5. For the CI adopter this *is* the onboarding, and it would have caught the 2.3/2.4/2.6 failure classes in one place.

### PR B — Discovery & onboarding
- **2.7 Daemon socket discovery:** `serve --port` binds `~/.loop/daemon-<port>.sock` (`client.go:53-66` resolves only `$LOOP_DAEMON_SOCK` → default) — the banner's own suggested command fails. **Fix:** banner prints the resolved socket + exact working commands (`--socket` path and the `LOOP_DAEMON_SOCK` export hint); the client's "no loop daemon" error globs `~/.loop/daemon-*.sock` and names live sockets it found ("found daemon-8787.sock — retry with `--socket …`") instead of claiming none exists (EU's middle ground: no registry, but the error must not lie about what's running). Manual §3 "you never type them" corrected. Tests: banner content; error text with a live port-daemon present.
- **2.8 `.env.sample` dead-end:** `setup.go:42-43` says `cp .env.sample .env`; curl installs ship no such file. **Fix:** embed the sample (go:embed); `loop setup` offers to write `.env` (interactive y/N; non-interactive prints exact contents). While in setup: validate that referenced env-var names match what the provider layer reads. Tests: fresh-HOME setup → `.env` created or exact instructions printed; no reference to a nonexistent file.

### PR C — Scaffold & docs accuracy *(must land before any Phase 3 copy rewrite — PO)*
- **2.9 `init` never writes root `README.md`; legacy demo retired (decisions D2 + D3, one PR — they touch the same file map):** `init.go:35` writes root `README.md`; the manual's documented `init && rm … README.md` (line 163) deletes the user's own README in existing repos (reproduced). **Fix:** scaffold README becomes `.loop/README.md`; add `--dry-run` (lists planned files, explicitly prints "root README: untouched" when one exists — the reassurance is the feature); the manual's `rm` one-liner is deleted. In the same PR, `leadership.yaml` and `feature-delivery.yaml` move out of the default scaffold into `examples/` (the manual already tells users to delete them — this resolves an existing incoherence). **`review.yaml` stays in the default scaffold — it is the adoption path.** Re-init is idempotent (no root README, ever). Tests: dirty dir → root README byte-identical; dry-run writes nothing.
- **2.10 Docs accuracy sweep:** `executor.go:39` help drops "requires Node 22+" (superseded by the standalone host — same claim in `examples/agency-intake/README.md`); manual §4 `loop executor doctor` → `loop doctor`; validate's "all problems at once" caveat (stage-type dispatch errors one at a time); CEO/founder framing unified; README's "six shipped personas" copy synced with D1 when 3.6 lands.

### PR D — Provider & cost honesty
- **2.11 Streaming usage (OpenAI-compatible) — PO launch-blocker #2:** `llm/openai_stream.go` never requests or parses usage → `/cost`, `state.json`, dashboard show 0 for Groq/Ollama/OpenRouter/vLLM. **Fix:** send `stream_options:{include_usage:true}`; parse the trailing usage frame; on a 400 **whose body plausibly names `stream_options`**, retry once without (a 400 precedes any streamed delta, so the retry is safe; retrying other 400s would hide root causes). Fix the sparse tool-call index reassembly in the same file. Tests: `httptest` SSE fixture with usage frame; targeted-400 fallback; sparse-index fixture (calls at indexes 0 and 2 — today's loop drops the trailing one).
- **2.12 Live-provider verification gate (PO, must):** a fixture proves the parser, not the provider. Phase 2 is not done until a **live smoke against Groq** (the provider the funnel copy names) shows a nonzero cost or an explicit "—", never 0 — and a **tested-provider matrix** is published (Groq + one more; untested providers labelled "unverified"). No "cost printed" claim ships in README/landing until this passes. **Internal ordering gate for PR D: 2.14's manual ledger publishes only after this passes.**
- **2.13 No-usage renders as "—", not 0:** everywhere usage renders (run page, `/cost`, room ledger) — absence shows "not reported", including runs that predate the ledger. A zero is a lie; a dash is honesty. Test: render with no usage event.
- **2.14 Cost transparency copy** *(folded in from v1's 3.7 — lands with the fixed meter)*: manual §6 publishes the real numbers, both the estimate ("a 4-persona room ≈ 5–7 calls/message") and the actual ledger from this repo's dev room (97 calls · 511K input · 42K output tokens); `/agents` shows the per-message estimate. *(If the launch window compresses, the `/agents` estimate is first to trim — the manual ledger alone satisfies the PO.)*

### PR E — Rooms & validate
- **2.15 Persona dir duplication:** `persona.go:170` lists `.loop/personas` twice. Preserve-order dedupe. Test: conventional layout → each dir once.
- **2.16 `validate` dead room refs — resolution made consistent (decision D4, amended):** runtime resolves room `pipelines: file:` **room-dir-relative only** (`roomrun.go:139`, `:264`); v1 proposed workspace-root-first validation, which would have made `validate` bless rooms `/run` then fails to resolve — a new lie (EU, must). **Ruling: room-dir-relative everywhere, now documented.** Validation resolves each `file:` with the *same* resolver `/run` uses, stats it, and validates the target; manual documents the room-relative rule. Test: the **same** file through both `validate` and the runtime resolver — identical result, both directions.
- **2.17 Room silence honesty (dashboard):** unaddressed message with a dead key says "no agent replied — address @someone" while the CLI logged the 401s. **Fix:** when all agents went silent due to errors, the room emits a transcript system event carrying the first error ("no agent replied: anthropic: HTTP 401 … (3 agents failed)"), rendered by the dashboard chat fragment. **Provider strip** on the room page + dashboard home distinguishes "key set" from "key works" (a green strip over a dead key is the lie class we're killing — show `anthropic: set · unverified` until first success). Tests: fake-server decision failure → error text in transcript event; strip renders both states.
- **2.18 Empty model name in errors:** `model : anthropic: HTTP 401` — when the model ID resolves from env, print the fallback (`model (from ANTHROPIC_API_KEY env)`). Test: formatter unit.

### PR F — Small UI honesty
- **2.19 WebUI vars input accepts spaces** *(pulled forward from v1's 3.5 — EU day-1 blocker, ~20 lines)*: quote-aware `k=v` parsing (spaces inside quotes; last `=` splits), replacing the `strings.Fields` split. Test: parser table. *(PO preferred deferring this past the launch window; kept because it is a ~20-line EU day-1 blocker riding PR F — first candidate to cut if the window compresses.)*

**Phase 2 exit criteria:** the EU's day-1 journey replays with zero dead-ends; the Groq live smoke passes and the provider matrix is published; the EU's twelve-item pain list is closed (their review scored the plan 85/100 with these changes, 70 without).

---

## Phase 3 — Product surface (after Phase 2)

- **3.1 Funnel inversion — explicitly setup-free:** README + landing lead with `loop run .loop/pipelines/review.yaml` (one diff + one LLM call, no executor, free-tier key works, cost printed — only after 2.12 passes). **Setup deferral is explicit (PO, must):** key → `loop run review.yaml` → `loop ask` → only when the user opts into agents writing code (`implement.yaml`) does `loop setup` appear, as a consented, explained step — today it is step 1 of the landing quickstart (`landing.tmpl:81`) and installs a toolchain before any value. Rooms second; implement third. *(The landing-demo swap — drone shoot-out → review run — is meter-independent and may land with 2.10's docs sweep.)*
- **3.2 Positioning:** adopt the PO one-pager into README top and website: category sentence, target user, 60-second demo, three-term vocabulary (pipeline/gate/room), the vs-Claude-Code sentence, honest-limitations list. Read as reordering the **landing hero and feature-card order** (both room-led today), not just the quickstart list.
- **3.3 Glossary:** one page in the manual; core terms pipeline/gate/room only; the rest demoted to the appendix; landing/README coinage sweep.
- **3.4 Machine-readable output:** `loop run --json` (streams `events.jsonl` lines to stdout) and `--dry-run` printing the stage plan + approximate model-call count (labelled approximate). *(Exit-code documentation already shipped in 2.6.)* Tests: golden JSON on a mock-provider pipeline. Warning/noise discipline: under `--json`, human warnings are suppressed on stdout (they'd corrupt the stream).
- **3.5 Shipped persona set (decision D1):** ship four (architect/engineer/reviewer/product-owner); `cfo`/`end-user` move to `examples/council/`. README's "six personas" copy syncs in 2.10.

*(The EU explicitly defers 3.2/3.3 — product-copy work that doesn't touch their journey; they stay pre-launch because the PO's green-light requires the positioning and honest-limitations list.)*

## Phase 4 — Explicitly not building now, and deferred with reasons

**Not building:** schedulers/triggers/webhooks · multi-project dashboard · auth/multi-user · plugin system · more personas · richer router conditions (until a real user asks) · daemon auto-discovery registry (2.7's glob-in-error is the adopted middle ground).

**Deferred, recorded so nothing is silently dropped (PSE):**
- **Gate-protocol drift** — gate sentinels scraped as prose exist in `cli/run.go:63-64` and `cli/run_daemon.go:69-70`, parsed by `runctl/translator.go`; one shared constant is the fix. Deferred to the next PR touching either file, with a pinned-string test.
- **Cycle guard counts steps, not cost** (`runner.go:247`) — a tight router loop can spend real money inside the 1000-step budget. Deferred; candidate: explicit self-target check + per-run spend ceiling.
- **`Supervisor.Shutdown` waits serially, 3s per child** (`runctl/supervisor.go:339-356`). Deferred; parallel wait.
- **P3 residue:** byte-slicing rune clips, polling full-file re-reads, `evalComparison` operator split, `validateTemplateRefs`' forward-reference rejection for exotic forward-jump loops — **accepted tradeoff, documented** in the uncommitted `refs.go` work; on record here so it's triage, not omission.

---

## Decisions (ruled after persona review)

| # | Decision | Ruling |
|---|----------|--------|
| D1 | Cut shipped personas to 4? | **Yes** — cfo/end-user to `examples/council/` (all three approve; cheaper rooms) |
| D2 | Retire legacy scaffold pipelines/rooms to `examples/`? | **Yes** — folded into 2.9's PR; `review.yaml` stays (EU condition); manual already told users to delete them (PO verification) |
| D3 | `init` writes `.loop/README.md`, never root README? | **Yes** — with `--dry-run` that prints "root README: untouched" |
| D4 | Room pipeline path resolution | **Room-dir-relative everywhere, documented** (v1's workspace-root-first would have made `validate` contradict the runtime — EU caught it; `roomrun.go:139,264`) |
| D5 | Unknown `--var` | **`loop run`: hard-fail exit 1; rooms: warn** (EU: CI doesn't read stderr; PSE concurs with per-surface split) |

## Sequencing

```
PR 0.1 (CI; may carry 1.3 if -race is red) ──► PR 1.1 (web hardening) ──► PRs 1.2–1.5 (parallel)
  └──► Phase 2 in order: A (CLI honesty) → B (discovery/onboarding) → C (scaffold/docs)
        → D (provider/cost + live Groq gate) → E (rooms/validate) → F (vars input)
             └──► Phase 3 in ~3 PRs: (3.1–3.3 story) (3.4 machine-readable) (3.5 personas)
```

## Launch gate (binary checklist — the artifact the PO signs)

Organic releases may proceed after Phase 1 + the live-provider check (2.12) without waiting for Phase 3. **HN/Reddit launch only when every line passes:**

1. `POST` with `Origin: http://evil.example` → **403** on every mutating route (the original probe).
2. `GET` with `Host: evil.example` → **403** (DNS rebinding closed).
3. Room message on a Groq key: `/cost` and `state.json` show a number or "—" — **never 0**.
4. `loop init` in a repo with an existing README → README byte-identical afterward.
5. `install.sh` with SHA256SUMS unreachable → hard-fails, installs nothing.
6. Landing quickstart completes top-to-bottom without `loop setup` and without executor install.
7. No drone shoot-out, no `leadership.yaml`/`feature-delivery.yaml` as first-run commands anywhere in README/landing.
8. `go test -race ./...` green in CI.
9. Documented exit codes and the CI recipe are present in manual §5 *(added so the promoted EU musts can't be skipped under launch pressure — PO)*.

## Success criteria

Re-run the three-persona review after Phase 2 and again at the launch gate. **Consensus on this plan (v3): all three personas APPROVE — PSE 95%, PO 93%, EU 91%** — with every one of their required changes incorporated and their non-blocking suggestions folded into the items above. The plan closes their findings when: the PSE's probes fail closed, the EU's twelve pains are closed with the CI recipe in the manual, and every launch-gate line passes.

---

## Appendix A — verified code anchors (2026-09-26; re-verified by plan reviewers)

| Anchor | Location |
|---|---|
| mux + route table (POSTs from `:159`; 16 POST routes) | `internal/webui/webui.go:149-185` |
| activity pill `innerHTML` sink; selector interpolation | `internal/webui/assets/ui.js:64`, `:449` |
| weak run ID / `MkdirAll` / append; ID call sites | `internal/engine/runlog.go:27,40,46`; `internal/cli/run.go:163`, `internal/daemon/server_runs.go:150`, `internal/runctl/supervisor.go:156` |
| unlocked transcript reads | `internal/chat/room.go:317,389,555-562`; call site `internal/chat/dive.go:152` |
| socket resolution (`LOOP_DAEMON_SOCK` → default only); socket-side `daemon` base URL | `internal/daemon/client.go:53-66`; `webui.go:144` |
| `cp .env.sample .env` instruction | `internal/cli/setup.go:42-43` |
| silent var assignment (room warns at `roomrun.go:192`) | `internal/cli/run.go:140` |
| room `file:` refs resolve **room-dir-relative at runtime** | `internal/cli/roomrun.go:139`, `:264` |
| unconditional `files:` line | `internal/cli/digest.go:63` |
| `executor install` "Node 22+" help; root `loop doctor` (covers executor health) | `internal/cli/executor.go:39`, `:63-71`, `:93-111` |
| init writes root `README.md` | `internal/cli/init.go:35` |
| persona dir listing | `internal/config/persona.go:170` |
| OpenAI stream: zero usage handling | `internal/llm/openai_stream.go` |
| installer checksum `|| true` (+ asset-line-absent fallthrough) | `install.sh:41-55` |
| manual's destructive `init && rm … README.md`; landing step 1 is `setup` | `USER MANUAL.md:163`; `website/landing.tmpl:81` |
| lexical-only `safePath`; overclaiming comment vs honest disclaimer | `internal/agent/tools.go:224`, `:146-149`, `:260` |
