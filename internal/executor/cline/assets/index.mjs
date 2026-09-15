#!/usr/bin/env node
// loop-cline-host — the Node sidecar between loop's Go engine and
// @cline/sdk. Speaks newline-delimited JSON over stdio:
//
//   stdin : one task line (see PROTOCOL below), then EOF ends the host
//   stdout: one event line per observation, ending in "done" or "error"
//   stderr: free-form diagnostics (loop captures a tail for failures)
//
// PROTOCOL (v1)
//   task  → {"type":"task","taskId":"…","instruction":"…","system":"…",
//            "cwd":"…","model":{"provider":"anthropic|openai","model":"…",
//            "apiKey":"…","baseUrl":"…","temperature":0.7,"maxTokens":4096},
//            "tools":["read_file",…],"maxTurns":10,"approval":"auto|ask"}
//   event ← {"type":"text","text":"…"}                       assistant delta
//         ← {"type":"tool_call","tool":"…","detail":"…"}     tool invocation
//         ← {"type":"tool_result","tool":"…","detail":"…"}   tool outcome
//         ← {"type":"notice","text":"…"}                     lifecycle notes
//         ← {"type":"done","output":"…"}                     final answer
//         ← {"type":"error","message":"…"}                   fatal failure
//
// The host is deliberately defensive about SDK event names: known ones are
// mapped, unknown ones are forwarded as notices so SDK drift degrades to
// chatty logs instead of breakage.

import { Agent } from "@cline/sdk";
import { createInterface } from "node:readline";

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function toClineProvider(model) {
  // loop's two provider families map onto cline's documented IDs. A
  // baseUrl redirects the matching API family (e.g. ANTHROPIC_BASE_URL
  // pointing at a gateway) — it must not switch the wire format.
  if (model.provider === "anthropic") {
    const p = { providerId: "anthropic" };
    if (model.baseUrl) p.baseUrl = model.baseUrl;
    return p;
  }
  if (model.baseUrl) {
    return { providerId: "openai-compatible", baseUrl: model.baseUrl };
  }
  return { providerId: "openai" };
}

async function runTask(task) {
  const provider = toClineProvider(task.model || {});
  const opts = {
    providerId: provider.providerId,
    modelId: task.model?.model,
    apiKey: task.model?.apiKey,
  };
  if (provider.baseUrl) opts.baseUrl = provider.baseUrl;
  if (task.model?.temperature != null) opts.temperature = task.model.temperature;
  if (task.model?.maxTokens) opts.maxTokens = task.model.maxTokens;
  if (task.maxTurns) opts.maxIterations = task.maxTurns;

  // v1 limitation (documented): the SDK exposes no system-prompt option, so
  // the persona travels inside the instruction, clearly delimited.
  const instruction = task.system
    ? `[OPERATING PRINCIPLES — follow strictly]\n${task.system}\n[/OPERATING PRINCIPLES]\n\n${task.instruction}`
    : task.instruction;

  if (task.approval === "auto") {
    send({ type: "notice", text: "approval=auto: this host version uses SDK default permissions" });
  }

  const agent = new Agent(opts);
  let failure = null;
  agent.subscribe((event) => {
    const t = event?.type || "";
    if (t === "assistant-text-delta") {
      send({ type: "text", text: event.text ?? "" });
    } else if (t.includes("tool_call") || t === "tool-use") {
      send({ type: "tool_call", tool: event.tool?.name || event.tool || "", detail: safeJSON(event) });
    } else if (t.includes("tool_result") || t === "tool-result") {
      send({ type: "tool_result", tool: event.tool?.name || event.tool || "", detail: safeJSON(event) });
    } else if (t === "run-failed") {
      // Empirical (SDK 0.0.82): failures arrive as run-failed events and
      // agent.run() still resolves — remember it and fail at the end.
      failure = [event?.snapshot?.lastError, event?.errorClass].filter(Boolean).join(" ") || "agent run failed";
    } else if (t === "error") {
      failure = event.message || event.error?.message || "SDK error event";
    } else {
      send({ type: "notice", text: `[${t}] ${safeJSON(event)}` });
    }
  });

  let result = null;
  try {
    result = await agent.run(instruction);
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

function safeJSON(v) {
  try {
    const s = JSON.stringify(v);
    return s.length > 2000 ? s.slice(0, 2000) + "…" : s;
  } catch {
    return String(v);
  }
}

// One task per host invocation keeps process lifetime simple and matches
// loop's model (a stage = a task = an auditable unit).
const rl = createInterface({ input: process.stdin });
let received = false;
rl.on("line", (line) => {
  if (received || !line.trim()) return;
  received = true;
  rl.close();
  let task;
  try {
    task = JSON.parse(line);
  } catch (e) {
    send({ type: "error", message: `malformed task line: ${e.message}` });
    process.exit(1);
  }
  runTask(task).then(
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
