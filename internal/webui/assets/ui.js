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
    function refresh() {
      window.htmx.ajax("GET", refreshURL, el);
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
    scrollChat();
  });

  // Chat views keep the newest message in view: after every swap into
  // the live region, and once on open.
  function scrollChat() {
    var log = document.querySelector(".chat-log");
    if (log) log.scrollTop = log.scrollHeight;
  }
  document.body.addEventListener("htmx:afterSwap", scrollChat);
})();
