// Fills the DNS resolver row with the address and company of the visitor's DNS resolver; a failure, a slow answer or no JavaScript leaves the dash.
(function () {
  var d = document.getElementById("dns");
  if (!d || !window.crypto || !crypto.getRandomValues) return;
  var t, u, k = 0;
  // A new one-time name: the resolver looks it up, and the request only causes that lookup (nginx answers it, the site is not asked).
  // share.js sends the token with Share, so the report can name the resolver.
  function get(p, m) { return fetch(u + p, Object.assign(m, { cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" })); }
  function go() {
    t = "";
    crypto.getRandomValues(new Uint8Array(16)).forEach(function (x) { t += (x < 16 ? "0" : "") + x.toString(16); });
    d.dataset.t = t;
    u = "https://" + t + "." + d.dataset.zone + "/";
    get("t", { mode: "no-cors" }).catch(function () {});
    step(0);
  }
  // The first name failed or stayed empty (a lost lookup, a wrong answer kept by the browser): one second name, then the dash.
  function next() { k++ ? stop() : go(); }
  function show(j) {
    var p = j && j.ip, n = j && j.name, o = j && j.org;
    if (typeof p != "string" || p.length > 45 || !/^[0-9a-fA-F:.]+$/.test(p)) return stop();
    d.textContent = "";
    var l = document.createElement("a");
    l.href = "/?" + p; l.textContent = p;
    d.appendChild(l);
    if (typeof n != "string" || !n || n.length > 120) return;
    var s = document.createElement("small");
    var e = s;
    if (typeof o == "string" && /^\/as(n-[a-z0-9-]{1,255}|[0-9]{1,10})$/.test(o)) {
      e = document.createElement("a");
      e.href = o;
      s.appendChild(e);
    }
    e.textContent = n;
    d.appendChild(s);
  }
  // Gave up: the waiting dots end, the dash shows.
  function stop() { d.classList.add("x"); }
  function step(i) {
    // Our DNS server reports lookups in batches, up to two seconds late: the first try is 2.2 s after the lookup, then every 2 s.
    // 8 tries on the first name (16 s: a lookup that arrived shows up within that), 30 on the second (a minute).
    setTimeout(function () {
      var c = new AbortController();
      setTimeout(function () { c.abort(); }, 3000);
      get("", { signal: c.signal })
        .then(function (r) {
          if (r.status == 200) return r.json().then(show);
          r.status == 404 && i + 1 < (k ? 30 : 8) ? step(i + 1) : next();
        })
        .catch(next);
    }, i ? 2000 : 2200);
  }
  document.readyState == "loading" ? document.addEventListener("DOMContentLoaded", go) : go();
})();
