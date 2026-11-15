// Fills the DNS row with the network of the visitor's DNS resolver; a failure, a slow answer or no JavaScript leaves the dash.
(function () {
  var d = document.getElementById("dns");
  if (!d || !window.crypto || !crypto.getRandomValues) return;
  var b = crypto.getRandomValues(new Uint8Array(16)), t = "", w = [0, 800, 1500, 2500];
  b.forEach(function (x) { t += (x < 16 ? "0" : "") + x.toString(16); });
  var u = "https://" + t + "." + d.dataset.zone + "/";
  function show(j) {
    var n = j && j.name, a = j && j.asn;
    n = typeof n == "string" && n.length <= 120 ? n : "";
    a = Number.isInteger(a) && a > 0 && a < 4294967296 ? a : 0;
    if (!n && !a) return;
    d.textContent = n;
    if (a) {
      var l = document.createElement("a");
      l.className = "as"; l.href = "/as" + a; l.textContent = "AS" + a;
      d.appendChild(l);
    }
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
  function go() { try { step(0); } catch (e) {} }
  document.readyState == "loading" ? document.addEventListener("DOMContentLoaded", go) : go();
})();
