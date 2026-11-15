// Fills the DNS resolver row with the address and company of the visitor's DNS resolver; a failure, a slow answer or no JavaScript leaves the dash.
(function () {
  var d = document.getElementById("dns");
  if (!d || !window.crypto || !crypto.getRandomValues) return;
  var t = "", n = 30;
  crypto.getRandomValues(new Uint8Array(16)).forEach(function (x) { t += (x < 16 ? "0" : "") + x.toString(16); });
  // share.js sends it with Share, so the report can name the resolver.
  d.dataset.t = t;
  var u = "https://" + t + "." + d.dataset.zone + "/";
  function show(j) {
    var p = j && j.ip, n = j && j.name, o = j && j.org;
    if (typeof p != "string" || p.length > 45 || !/^[0-9a-fA-F:.]+$/.test(p)) return stop();
    d.textContent = "";
    var l = document.createElement("a");
    l.href = "/?" + p; l.textContent = p;
    d.appendChild(l);
    if (typeof n != "string" || !n || n.length > 120) return;
    var e = document.createElement(typeof o == "string" && /^\/as(n-[a-z0-9-]{1,255}|[0-9]{1,10})$/.test(o) ? "a" : "span");
    e.className = "dn"; e.textContent = n;
    if (e.tagName == "A") e.href = o;
    d.appendChild(e);
  }
  // Gave up: the waiting dots end, the dash shows.
  function stop() { d.classList.add("x"); }
  function step(i) {
    // 30 tries: the first 2.2 s after the lookup, then every 2 s, about a minute in all.
    setTimeout(function () {
      var c = new AbortController();
      setTimeout(function () { c.abort(); }, 3000);
      fetch(u, { signal: c.signal, cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" })
        .then(function (r) {
          if (r.status == 404 && i + 1 < n) return step(i + 1);
          if (r.status == 200) return r.json().then(show);
          stop();
        })
        .catch(stop);
    }, i ? 2000 : 2200);
  }
  // The first request only makes the resolver look the name up (nginx answers it, the site is not asked).
  // Our DNS server reports lookups in batches, up to two seconds late, so the answer is asked for after that.
  function go() {
    try {
      fetch(u + "t", { mode: "no-cors", cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" }).catch(function () {});
      step(0);
    } catch (e) {}
  }
  document.readyState == "loading" ? document.addEventListener("DOMContentLoaded", go) : go();
})();
