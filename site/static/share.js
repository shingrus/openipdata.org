// The Share button; hidden until this runs. A click makes the report (POST /share), once, and copies its link.
(function () {
  var b = document.querySelector(".sh");
  if (!b) return;
  b.hidden = false;
  var s = b.lastChild, busy = false, link = "", tx = s.textContent;
  // A new label on the button for 1.5 s.
  function flash(text) {
    clearTimeout(b.k);
    s.textContent = text;
    b.k = setTimeout(function () { s.textContent = tx; }, 1500);
  }
  function put(u) { return navigator.clipboard.writeText(u); }
  // The link of the shared report: made by the server on the first click (POST /share), then reused.
  function share() {
    if (link) return Promise.resolve(link);
    var d = document.getElementById("dns"), t = d ? d.dataset.t || "" : "", c = new AbortController();
    setTimeout(function () { c.abort(); }, 5000);
    return fetch("/share", { method: "POST", signal: c.signal, cache: "no-store", referrerPolicy: "no-referrer",
      headers: { "X-Share": "1", "Content-Type": "application/x-www-form-urlencoded" }, body: /^[0-9a-f]{32}$/.test(t) ? "t=" + t : "" })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(); })
      .then(function (j) {
        if (!j || typeof j.url != "string" || !/^https?:\/\/[^\s]{1,300}$/.test(j.url) || !/^[0-9a-f]{32}$/.test(j.id)) return Promise.reject();
        link = j.url;
        var o = document.getElementById("other");
        // The other family's address reaches the report through the server.
        if (o && o.dataset.from) {
          try {
            var u = new URL(o.dataset.from, location.href);
            u.searchParams.set("r", j.id);
            fetch(u, { mode: "no-cors", cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" }).catch(function () {});
          } catch (e) {}
        }
        return link;
      });
  }
  b.onclick = function () {
    if (busy) return;
    busy = true;
    s.textContent = "Copying...";
    var p = share(), w;
    // Safari only lets the click itself write to the clipboard: hand it the promise of the link.
    try {
      w = window.ClipboardItem && navigator.clipboard.write
        ? navigator.clipboard.write([new ClipboardItem({ "text/plain": p.then(function (u) { return new Blob([u], { type: "text/plain" }); }) })])
            .catch(function () { return p.then(put); })
        : p.then(put);
    } catch (x) { w = p.then(put); }
    w.then(function () { busy = false; flash("Link copied"); }, function () {
      busy = false;
      // The report exists but the browser refused the clipboard: show the link so it can be copied by hand.
      if (link) { try { window.prompt("Copy the link", link); } catch (x) {} }
      flash(link ? "Share" : "Copy failed");
    });
  };
})();
