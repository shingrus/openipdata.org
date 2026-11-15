// Fills the DNS resolver row with the address and company of the visitor's DNS resolver; a failure, a slow answer or no JavaScript leaves the dash.
(function () {
  var d = document.getElementById("dns");
  if (!d || !window.crypto || !crypto.getRandomValues) return;
  var b = crypto.getRandomValues(new Uint8Array(16)), t = "", w = [2200, 2000, 2000];
  b.forEach(function (x) { t += (x < 16 ? "0" : "") + x.toString(16); });
  var u = "https://" + t + "." + d.dataset.zone + "/";
  function show(j) {
    var p = j && j.ip, n = j && j.name, o = j && j.org;
    if (typeof p != "string" || p.length > 45 || !/^[0-9a-fA-F:.]+$/.test(p)) return;
    d.textContent = "";
    var l = document.createElement("a");
    l.className = "lnk"; l.href = "/?" + p; l.textContent = p;
    d.appendChild(l);
    if (typeof n != "string" || !n || n.length > 120) return;
    var e = document.createElement(typeof o == "string" && /^\/as(n-[a-z0-9-]{1,255}|[0-9]{1,10})$/.test(o) ? "a" : "span");
    e.className = "dn"; e.textContent = n;
    if (e.tagName == "A") e.href = o;
    d.appendChild(e);
  }
  function step(i) {
    setTimeout(function () {
      var c = new AbortController();
      setTimeout(function () { c.abort(); }, 3000);
      fetch(u, { signal: c.signal, cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" })
        .then(function (r) {
          if (r.status == 404 && i + 1 < w.length) return step(i + 1);
          if (r.status == 200) return r.json().then(show);
        })
        .catch(function () {});
    }, w[i]);
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
