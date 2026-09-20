#!/usr/bin/env node
// loop-cline-host — the Node sidecar between loop's Go engine and
// @cline/sdk. Speaks newline-delimited JSON over stdio:
//
//   stdin : one task line, then (only when approval=ask) approval
//           response lines; EOF ends the host
//   stdout: one event line per observation, ending in "done" or "error"
//   stderr: free-form diagnostics (loop captures a tail for failures)
//
// PROTOCOL (v2)
//   task    → {"type":"task","taskId":"…","instruction":"…","system":"…",
//              "cwd":"…","model":{"provider":"anthropic|openai","model":"…",
//              "apiKey":"…","baseUrl":"…","temperature":0.7,"maxTokens":4096},
//              "tools":["read_file",…],"maxTurns":10,"approval":"auto|ask"}
//   event   ← {"type":"text","text":"…"}                       assistant delta
//           ← {"type":"tool_call","tool":"…","detail":"…"}     tool invocation
//           ← {"type":"tool_result","tool":"…","detail":"…"}   tool outcome
//           ← {"type":"usage","input":123,"output":45}         cumulative tokens
//           ← {"type":"notice","text":"…"}                     lifecycle notes
//           ← {"type":"approval_request","requestId":"…",      ask mode only:
//               "tool":"…","detail":"…"}                        run_commands
//   approval→ {"type":"approval_response","requestId":"…",      the answer to
//               "approved":true,"reason":"…"}                   a request above
//   event   ← {"type":"done","output":"…"}                     final answer
//           ← {"type":"error","message":"…"}                   fatal failure
//
// The host is deliberately defensive about SDK event names: known ones are
// mapped, unknown ones are forwarded as notices so SDK drift degrades to
// chatty logs instead of breakage.

import { Agent, createBuiltinTools, createDefaultExecutors } from "@cline/sdk";
import { createInterface } from "node:readline";
import { isAbsolute, normalize, resolve, relative, sep } from "node:path";

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

// confinedFileRead wraps the default file-read executor with the room
// loop's path rule: relative paths only, no `..` escapes, resolved
// against the task cwd. The builtin resolves absolute paths as-is and
// relative paths against process.cwd() with no confinement option —
// workspace reads must not escape by accident.
function confinedFileRead(cwd, defaultRead) {
  return async (request, context) => {
    const p = String(request.path ?? "").trim();
    if (p === "") {
      throw new Error("empty path");
    }
    if (isAbsolute(p)) {
      throw new Error(`path ${JSON.stringify(p)} must be relative to the workspace`);
    }
    const clean = normalize(p);
    if (clean === ".." || clean.startsWith(".." + sep)) {
      throw new Error(`path ${JSON.stringify(p)} escapes the workspace`);
    }
    // Absolutes pass the builtin's guard unchanged, so hand it the
    // already-validated absolute path.
    return defaultRead({ ...request, path: resolve(cwd, clean) }, context);
  };
}

function toClineProvider(model) {
  // loop's two provider families map onto cline's documented IDs. A
  // baseUrl redirects the matching API family (e.g. ANTHROPIC_BASE_URL
  // pointing at a gateway) — it must not switch the wire format.
  if (model.provider === "anthropic") {
    const p = { providerId: "anthropic" };
    if (model.baseUrl) {
      // loop's convention (like ANTHROPIC_BASE_URL and loop's own
      // adapter) is a base WITHOUT /v1: requests go to <base>/v1/messages.
      // The SDK's anthropic provider appends /messages directly, so it
      // needs the /v1 folded in — otherwise every call 404s behind a
      // 200 status and looks like "stream ended without a finish chunk".
      p.baseUrl = model.baseUrl.replace(/\/+$/, "").replace(/\/v1$/, "") + "/v1";
    }
    return p;
  }
  if (model.baseUrl) {
    return { providerId: "openai-compatible", baseUrl: model.baseUrl };
  }
  return { providerId: "openai" };
}

// loop's tool vocabulary maps onto the SDK's builtin tools. read_file
// brings search_codebase along: both are read-only and search is how an
// agent orients before opening files.
const TOOL_MAP = {
  read_file: ["read_files", "search_codebase"],
  write_file: ["editor"],
  run_command: ["run_commands"],
};

// allowedTools resolves the task's tool list into SDK builtin names.
// Unknown entries become notices (returned as strings) instead of
// silently vanishing: a typo in a pipeline's tools list must be visible
// in the run log.
function allowedTools(tools) {
  const allowed = new Set();
  const unknown = [];
  for (const name of tools || []) {
    const mapped = TOOL_MAP[name];
    if (mapped) {
      for (const t of mapped) allowed.add(t);
    } else {
      unknown.push(name);
    }
  }
  if (allowed.size === 0) {
    // No tools declared = executor default: read-only orientation.
    allowed.add("read_files");
    allowed.add("search_codebase");
  }
  return { allowed, unknown };
}

// buildToolPolicies encodes the approval contract. Read and write inside
// the workspace are always auto-approved; shell commands are too under
// approval=auto, and go to the human under approval=ask.
function buildToolPolicies(allowed, approval) {
  const policies = {};
  for (const name of allowed) {
    policies[name] =
      approval === "ask" && name === "run_commands"
        ? { enabled: true, autoApprove: false }
        : { enabled: true, autoApprove: true };
  }
  return policies;
}

// ApprovalBroker bridges the SDK's async requestToolApproval callback to
// the stdio protocol: requests go out as lines, responses come back as
// lines, matched by requestId.
class ApprovalBroker {
  constructor(rl) {
    this.rl = rl;
    this.pending = new Map();
    this.nextId = 0;
    rl.on("line", (line) => {
      let msg;
      try {
        msg = JSON.parse(line);
      } catch {
        return; // not protocol; ignore
      }
      if (msg?.type !== "approval_response" || !msg.requestId) return;
      const resolve = this.pending.get(msg.requestId);
      if (!resolve) return;
      this.pending.delete(msg.requestId);
      resolve({
        approved: msg.approved === true,
        reason: msg.reason || (msg.approved === true ? "" : "denied by the operator"),
      });
    });
  }

  // ask resolves when loop sends the operator's answer. stdin closing
  // while a request is pending means the runner went away: deny so the
  // run ends deterministically instead of hanging forever.
  request(tool, input) {
    const requestId = `ap${++this.nextId}`;
    return new Promise((resolve) => {
      this.pending.set(requestId, resolve);
      this.rl.once("close", () => {
        if (this.pending.delete(requestId)) {
          resolve({ approved: false, reason: "the runner disconnected before answering" });
        }
      });
      send({ type: "approval_request", requestId, tool, detail: safeJSON(input) });
    });
  }
}

async function runTask(task, rl) {
  const provider = toClineProvider(task.model || {});
  const { allowed, unknown } = allowedTools(task.tools);
  for (const name of unknown) {
    send({ type: "notice", text: `unknown tool ${JSON.stringify(name)} in tools list — skipped (known: read_file, write_file, run_command)` });
  }

  // Tools: the builtin set (workspace-confined reads, 2-minute shell)
  // filtered to the task's allow-list by allowedTools above.
  const executors = createDefaultExecutors({ bash: { timeoutMs: 120_000 } });
  const opts = {
    providerId: provider.providerId,
    modelId: task.model?.model,
    apiKey: task.model?.apiKey,
    // The persona's operating principles belong in the system prompt,
    // where the SDK puts them, not smuggled into the instruction.
    systemPrompt: task.system || "",
    tools: createBuiltinTools({ cwd: task.cwd || process.cwd() }).filter((t) =>
      allowed.has(t.name)
    ),
    toolPolicies: buildToolPolicies(allowed, task.approval),
    executors: {
      ...executors,
      fileRead: confinedFileRead(task.cwd || process.cwd(), executors.fileRead),
    },
  };
  if (provider.baseUrl) opts.baseUrl = provider.baseUrl;
  if (task.model?.temperature != null) opts.temperature = task.model.temperature;
  if (task.model?.maxTokens) opts.maxTokensPerTurn = task.model.maxTokens;
  if (task.maxTurns) opts.maxIterations = task.maxTurns;
  if (task.approval === "ask") {
    const broker = new ApprovalBroker(rl);
    opts.requestToolApproval = (request) => broker.request(request.toolName, request.input);
  }

  const agent = new Agent(opts);
  let failure = null;
  // SDK events that carry no timeline value: per-token or per-message
  // state snapshots whose payload already reaches the host through the
  // text / usage / done lines. Forwarding them as notices once flooded
  // the audit log — 40+ snapshot dumps for one ordinary agent stage.
  const ignore = new Set([
    "run-started", "run-finished", "turn-started", "turn-finished",
    "message-added", "assistant-message", "assistant-reasoning-delta",
    "reasoning-delta", "tool-call-delta", "tool-updated",
    "tool-call", "tool-started",
  ]);
  agent.subscribe((event) => {
    const t = event?.type || "";
    if (t === "assistant-text-delta") {
      send({ type: "text", text: event.text ?? "" });
    } else if (t === "tool-result" || t === "tool-finished") {
      // One line per completed tool call. This SDK build emits
      // tool-finished as a whole-runtime snapshot — the tool's name
      // lives only in the newest tool part inside it — hence the dig.
      send({ type: "tool_result", tool: toolNameOf(event), detail: safeJSON(event.output ?? event) });
    } else if (t === "usage-updated") {
      // Cumulative after each model call — forward the totals; the Go
      // side diffs consecutive events into its meter.
      const u = event.usage || {};
      send({ type: "usage", input: u.inputTokens ?? 0, output: u.outputTokens ?? 0 });
    } else if (t === "run-failed") {
      // Empirical (SDK 0.0.82): failures arrive as run-failed events and
      // agent.run() still resolves — remember it and fail at the end.
      failure = [event?.snapshot?.lastError, event?.errorClass].filter(Boolean).join(" ") || "agent run failed";
    } else if (t === "error") {
      failure = event.message || event.error?.message || "SDK error event";
    } else if (!ignore.has(t)) {
      // Unknown event types still surface — a new SDK event should be
      // visible, not silently dropped — but known-noise ones never dump
      // their snapshots into the log.
      send({ type: "notice", text: `[${t}] ${safeJSON(event)}` });
    }
  });

  let result = null;
  try {
    result = await agent.run(task.instruction);
  } catch (err) {
    failure = failure || err?.stack || String(err);
  }
  if (failure) {
    send({ type: "error", message: failure });
    return;
  }
  // The SDK's result shape is not fully documented; extract defensively.
  const output =
    result?.text ?? result?.completion ?? result?.message ??
    (typeof result === "string" ? result : "");
  send({ type: "done", output });
}

// toolNameOf digs the tool's name out of an SDK tool event. Top-level
// fields when present (tool-result per the type docs); otherwise the
// newest tool part in the runtime snapshot (what this build actually
// emits for tool-finished).
function toolNameOf(event) {
  if (typeof event?.toolName === "string" && event.toolName) return event.toolName;
  if (typeof event?.toolCall?.name === "string" && event.toolCall.name) return event.toolCall.name;
  if (typeof event?.tool?.name === "string" && event.tool.name) return event.tool.name;
  const msgs = event?.snapshot?.messages || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const parts = msgs[i]?.content;
    if (!Array.isArray(parts)) continue;
    for (const p of parts) {
      if (typeof p?.toolName === "string" && p.toolName) return p.toolName;
    }
  }
  return "";
}

function safeJSON(v) {
  try {
    const s = JSON.stringify(v);    return s.length > 2000 ? s.slice(0, 2000) + "…" : s;
  } catch {
    return String(v);
  }
}

// One task per host invocation keeps process lifetime simple and matches
// loop's model (a stage = a task = an auditable unit). stdin stays open
// after the task line: approval=ask runs receive response lines on it.
const rl = createInterface({ input: process.stdin });
let received = false;
rl.on("line", (line) => {
  if (!line.trim()) return;
  if (received) return; // task already running; only approval responses expected
  received = true;
  let task;
  try {
    task = JSON.parse(line);
  } catch (e) {
    send({ type: "error", message: `malformed task line: ${e.message}` });
    process.exit(1);
  }
  runTask(task, rl).then(
    () => process.exit(0),
    (err) => {
      send({ type: "error", message: err?.stack || String(err) });
      process.exit(1);
    }
  );
});
rl.on("close", () => {
  if (!received) {
    send({ type: "error", message: "no task received on stdin" });
    process.exit(1);
  }
});
