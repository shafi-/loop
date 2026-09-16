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

  // ─── @-mention autocomplete (chat-app behavior) ────────────────────
  // Typing "@" in the composer opens a participant menu above the
  // input, filtered by what follows; ↑/↓ move, Enter/Tab accept,
  // Esc closes. Accepting splices "@name " in at the caret.
  function initMentions() {
    document.querySelectorAll(".composer form[data-agents]").forEach(bind);
  }

  function bind(form) {
    var input = form.querySelector("input[name='text']");
    var menu = form.querySelector(".mention-menu");
    if (!input || !menu || input.dataset.mentionsBound) return;
    input.dataset.mentionsBound = "1";
    var agents = (form.getAttribute("data-agents") || "").split(/\s+/).filter(Boolean);
    var items = [], sel = -1;

    function close() {
      menu.hidden = true;
      menu.innerHTML = "";
      items = [];
      sel = -1;
    }
    function open(fragment) {
      var matches = agents.filter(function (a) { return a.indexOf(fragment.toLowerCase()) === 0; });
      if (!matches.length) return close();
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
    function fragment() {
      var caret = input.selectionStart || 0;
      var m = input.value.slice(0, caret).match(/(^|\s)@([a-z0-9_-]*)$/i);
      return m ? m[2] : null;
    }
    input.addEventListener("input", function () {
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
        // Consume the key so the form does not submit mid-mention.
        e.preventDefault();
        e.stopPropagation();
        accept(items[sel] ? items[sel].textContent.slice(1) : agents[0]);
      } else if (e.key === "Escape") {
        close();
      }
    });
    document.addEventListener("click", function (e) {
      if (!form.contains(e.target)) close();
    });
  }

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
    var text = (e.detail.xhr.responseText || "").replace(/<[^>]*>/g, " ").trim();
    toast("✗ " + (text || "HTTP " + e.detail.xhr.status));
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
