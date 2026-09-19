// loop ui — live run body over the daemon's SSE stream.
// The daemon pushes each events.jsonl line as an SSE message; every
// message (debounced) refetches the timeline fragment via htmx. The
// named "end" event closes the stream: the run reached a terminal
// state and the log went quiet.
(function () {
  "use strict";

  function connect(el) {
    var url = el.getAttribute("data-sse-connect");
    var refreshURL = el.getAttribute("data-refresh");
    if (!url || !refreshURL || !window.EventSource || !window.htmx) return;

    var timer = null;
    var deferred = 0;
    function refresh() {
      // A composer send is in flight: never supersede it with a
      // background refresh (htmx aborts superseded same-target
      // requests). Retry shortly — but cap the deferral so a missed
      // event can never stall the live region.
      if (composerInFlight > 0 && deferred < 6) {
        deferred++;
        timer = setTimeout(function () {
          timer = null;
          refresh();
        }, 300);
        return;
      }
      deferred = 0;
      window.htmx.ajax("GET", refreshURL, el).then(function () {
        // htmx.ajax swaps don't auto-process the new content (unlike
        // attribute-triggered requests): without this, a form swapped
        // into the log — the gate card's answer form — submits natively
        // and navigates away. Idempotent on already-processed nodes.
        window.htmx.process(el);
      });
    }
    var es = new EventSource(url);
    es.onmessage = function () {
      if (timer) return; // events arrive in bursts; refresh once per burst
      timer = setTimeout(function () {
        timer = null;
        refresh();
      }, 450);
    };
    es.addEventListener("end", function () {
      refresh();
      es.close();
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-sse-connect]").forEach(connect);
    initMentions();
    // Opening the page lands on the newest message.
    var log = document.querySelector(".chat-log");
    if (log) log.scrollTop = log.scrollHeight;
  });

  // ─── @-mention autocomplete & /-command palette ────────────────────
  // Typing "@" in the composer opens a participant menu above the
  // input; typing "/" as the whole line opens the room's command
  // palette. Both: filtered by what follows; ↑/↓ move, Enter/Tab
  // accept, Esc closes. Mentions splice "@name " at the caret;
  // commands replace the line with "/verb ".
  function initMentions() {
    document.querySelectorAll(".composer form[data-agents]").forEach(bind);
  }

  function bind(form) {
    var input = form.querySelector("input[name='text']");
    var menu = form.querySelector(".mention-menu");
    if (!input || !menu || input.dataset.mentionsBound) return;
    input.dataset.mentionsBound = "1";
    var agents = (form.getAttribute("data-agents") || "").split(/\s+/).filter(Boolean);
    var commands = [];
    try { commands = JSON.parse(form.getAttribute("data-commands") || "[]"); } catch (e) {}
    var pipelines = [];
    try { pipelines = JSON.parse(form.getAttribute("data-pipelines") || "[]"); } catch (e) {}
    var items = [], sel = -1, mode = null; // "mention" | "command" | "pipeline"

    function close() {
      menu.hidden = true;
      menu.innerHTML = "";
      items = [];
      sel = -1;
      mode = null;
    }
    function open(fragment) {
      var matches = agents.filter(function (a) { return a.indexOf(fragment.toLowerCase()) === 0; });
      if (!matches.length) return close();
      mode = "mention";
      menu.innerHTML = "";
      items = matches.map(function (name) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "mention-item";
        b.textContent = "@" + name;
        b.addEventListener("mousedown", function (e) { e.preventDefault(); accept(name); });
        menu.appendChild(b);
        return b;
      });
      sel = 0;
      items[0].classList.add("sel");
      menu.hidden = false;
    }
    function openCommands(fragment) {
      var matches = commands.filter(function (c) {
        return c.name.indexOf(fragment.toLowerCase()) === 0;
      });
      if (!matches.length) return close();
      mode = "command";
      menu.innerHTML = "";
      items = matches.map(function (cmd) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "mention-item cmd-item";
        var n = document.createElement("span");
        n.className = "cmd-name";
        n.textContent = "/" + cmd.name;
        var d = document.createElement("span");
        d.className = "cmd-desc";
        d.textContent = cmd.desc || "";
        b.appendChild(n);
        b.appendChild(d);
        b.addEventListener("mousedown", function (e) { e.preventDefault(); acceptCommand(cmd.name); });
        menu.appendChild(b);
        return b;
      });
      sel = 0;
      items[0].classList.add("sel");
      menu.hidden = false;
    }
    function accept(name) {
      var caret = input.selectionStart || 0;
      var at = input.value.slice(0, caret).lastIndexOf("@");
      if (at < 0) return close();
      input.value = input.value.slice(0, at) + "@" + name + " " + input.value.slice(caret);
      var pos = at + name.length + 2;
      input.setSelectionRange(pos, pos);
      input.focus();
      close();
    }
    function acceptCommand(verb) {
      input.value = "/" + verb + " ";
      input.setSelectionRange(input.value.length, input.value.length);
      input.focus();
      close();
    }
    // The /run second level: after the verb comes a pipeline name.
    function openPipelines(fragment) {
      var frag = fragment.toLowerCase();
      var matches = pipelines.filter(function (p) {
        return p.name.toLowerCase().indexOf(frag) === 0;
      });
      if (!matches.length) return close();
      mode = "pipeline";
      menu.innerHTML = "";
      items = matches.map(function (p) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "mention-item cmd-item";
        var n = document.createElement("span");
        n.className = "cmd-name";
        n.textContent = p.name;
        var d = document.createElement("span");
        d.className = "cmd-desc";
        d.textContent = p.name.indexOf("/") >= 0 ? "workspace pipeline" : "room pipeline";
        b.appendChild(n);
        b.appendChild(d);
        b.addEventListener("mousedown", function (e) { e.preventDefault(); acceptPipeline(p.name); });
        menu.appendChild(b);
        return b;
      });
      sel = 0;
      items[0].classList.add("sel");
      menu.hidden = false;
    }
    function acceptPipeline(name) {
      input.value = "/run " + name + " ";
      input.setSelectionRange(input.value.length, input.value.length);
      input.focus();
      close();
    }
    function fragment() {
      var caret = input.selectionStart || 0;
      var m = input.value.slice(0, caret).match(/(^|\s)@([a-z0-9_-]*)$/i);
      return m ? m[2] : null;
    }
    // The command palette is line-shaped: only when the whole input is
    // a slash word ("/", "/co…") — mid-message slashes stay prose.
    function commandFragment() {
      var m = input.value.match(/^\/([a-z0-9-]*)$/i);
      return m ? m[1] : null;
    }
    // "/run " plus a partial name opens the pipeline picker.
    function runFragment() {
      var m = input.value.match(/^\/run\s+([^\s]*)$/i);
      return m ? m[1] : null;
    }
    input.addEventListener("input", function () {
      var cmd = commandFragment();
      if (cmd !== null) { openCommands(cmd); return; }
      var run = runFragment();
      if (run !== null) { openPipelines(run); return; }
      var frag = fragment();
      if (frag === null) close(); else open(frag);
    });
    input.addEventListener("keydown", function (e) {
      if (menu.hidden) return;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        if (items[sel]) items[sel].classList.remove("sel");
        sel = (sel + (e.key === "ArrowDown" ? 1 : items.length - 1)) % items.length;
        items[sel].classList.add("sel");
      } else if (e.key === "Enter" || e.key === "Tab") {
        // Consume the key so the form does not submit mid-pick.
        e.preventDefault();
        e.stopPropagation();
        if (mode === "command") {
          var verb = items[sel] ? items[sel].querySelector(".cmd-name").textContent.slice(1) : commands[0].name;
          acceptCommand(verb);
        } else if (mode === "pipeline") {
          var pick = items[sel] ? items[sel].querySelector(".cmd-name").textContent : pipelines[0].name;
          acceptPipeline(pick);
        } else {
          accept(items[sel] ? items[sel].textContent.slice(1) : agents[0]);
        }
      } else if (e.key === "Escape") {
        close();
      }
    });
    document.addEventListener("click", function (e) {
      if (!form.contains(e.target)) close();
    });
  }

  // ─── pickers: a select fills its target path input ──────────────────
  // Delegated so selects swapped in later (fragments) work too.
  // "custom path…" hands the input back to the user, empty.
  document.addEventListener("change", function (e) {
    var sel = e.target.closest("select[data-fills]");
    if (!sel) return;
    var input = document.getElementById(sel.getAttribute("data-fills"));
    if (!input) return;
    if (sel.value === "__custom__") {
      input.value = "";
      input.focus();
    } else {
      input.value = sel.value;
    }
  });

  // After a picker refresh swap, aim the paired path input at the new
  // first option — the refresh exists because the lists changed.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (!t || !t.querySelector) return;
    var sel = t.querySelector("select[data-fills]");
    if (!sel) return;
    var input = document.getElementById(sel.getAttribute("data-fills"));
    if (input && sel.value && sel.value !== "__custom__") input.value = sel.value;
  });

  // Chat auto-scroll follows the reader: snap to the newest message
  // only when the reader is already near the bottom — scrolling up to
  // read history must never be yanked back by arriving events. The
  // reader's own sends always snap (they typed at the bottom).
  var CHAT_STICK_PX = 120;
  var chatStick = true;
  var forceStick = false; // the reader's own send: snap regardless of position
  var composerInFlight = 0;

  function nearBottom(log) {
    return log.scrollHeight - log.scrollTop - log.clientHeight <= CHAT_STICK_PX;
  }

  document.body.addEventListener("htmx:beforeRequest", function (e) {
    var f = e.detail.elt;
    if (f && f.matches && f.matches(".composer form")) {
      forceStick = true;
      composerInFlight++;
    }
  });
  document.body.addEventListener("htmx:afterRequest", function (e) {
    var f = e.detail.elt;
    if (f && f.matches && f.matches(".composer form")) {
      composerInFlight = Math.max(0, composerInFlight - 1);
      forceStick = false;
    }
  });
  document.body.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail.target;
    if (t && t.id === "chat-log") chatStick = forceStick || nearBottom(t);
  });
  document.body.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (t && t.id === "chat-log" && chatStick) t.scrollTop = t.scrollHeight;
  });

  // Failed requests must never be invisible: surface the server's
  // message as a toast (the room is busy, a gate closed, a halt
  // refused — all actionable). Inputs stay untouched for a retry.
  var toastTimer = null;
  function toast(msg) {
    var t = document.getElementById("toast");
    if (!t) {
      t = document.createElement("div");
      t.id = "toast";
      document.body.appendChild(t);
    }
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, 4500);
  }
  document.body.addEventListener("htmx:responseError", function (e) {
    var f = e.detail.elt;
    // The save form's 409 (file exists) is handled by its own confirm
    // flow below — a toast behind a modal would just be noise.
    if (f && f.matches && f.matches(".save-form") && e.detail.xhr.status === 409) return;
    var text = (e.detail.xhr.responseText || "").replace(/<[^>]*>/g, " ").trim();
    toast("✗ " + (text || "HTTP " + e.detail.xhr.status));
  });

  // ─── create page ────────────────────────────────────────────────────
  // Drafting replaces the editor's content; if something is already
  // there, ask before it vanishes.
  document.body.addEventListener("htmx:beforeRequest", function (e) {
    var f = e.detail.elt;
    if (!f || !f.matches || !f.matches(".draft-form")) return;
    var ed = document.getElementById("yaml");
    if (ed && ed.value.trim() && !window.confirm("Drafting replaces the editor's current content. Continue?")) {
      e.preventDefault();
    }
  });
  // Saving onto an existing file: the daemon refuses with 409; ask the
  // user and retry with the overwrite flag set. The daemon still
  // re-validates before writing.
  document.body.addEventListener("htmx:responseError", function (e) {
    var f = e.detail.elt;
    if (!f || !f.matches || !f.matches(".save-form") || e.detail.xhr.status !== 409) return;
    var msg = (e.detail.xhr.responseText || "").replace(/<[^>]*>/g, " ").trim();
    if (!window.confirm(msg + "\n\nOverwrite it?")) return;
    var flag = f.querySelector("input[name='overwrite']");
    if (flag) flag.value = "1";
    window.htmx.trigger(f, "submit");
  });
  // A completed save resets the flag — the next save asks again.
  document.body.addEventListener("htmx:afterRequest", function (e) {
    var f = e.detail.elt;
    if (!e.detail.successful || !f || !f.matches || !f.matches(".save-form")) return;
    var flag = f.querySelector("input[name='overwrite']");
    if (flag) flag.value = "0";
  });

  // The composer sits outside the swap region, so htmx never replaces
  // it — clear it after a successful send instead, and keep focus ready
  // for the next message.
  document.body.addEventListener("htmx:afterRequest", function (e) {
    var f = e.detail.elt;
    if (!e.detail.successful || !f.matches || !f.matches(".composer form")) return;
    f.reset();
    var input = f.querySelector("input[name='text']");
    if (input) input.focus();
  });

  // The sidecar polls and re-renders every few seconds; remember which
  // pipeline entries the user expanded and keep them expanded.
  document.body.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail.target;
    if (t && t.id === "sidecar") {
      window.__openRuns = Array.prototype.map.call(
        t.querySelectorAll("details[open]"),
        function (d) { return d.getAttribute("data-run-id"); }
      );
    }
  });
  document.body.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (t && t.id === "sidecar" && window.__openRuns) {
      window.__openRuns.forEach(function (id) {
        var d = t.querySelector('.sidecar-item[data-run-id="' + id + '"]');
        if (d) d.open = true;
      });
      window.__openRuns = null;
    }
  });
})();
