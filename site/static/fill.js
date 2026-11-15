// Fills the row of the other address family; a failed, slow (4 s) or implausible answer shows none (until then, and without JavaScript, the dash).
(function () {
  var d = document.getElementById("other");
  if (!d) return;
  var f = d.dataset.fam, u = d.dataset.from, v = d.querySelector(".v"), b = d.querySelector(".cp");
  function ok(s) {
    if (f == "4") {
      var p = s.split(".");
      return p.length == 4 && p.every(function (x) { return /^\d{1,3}$/.test(x) && +x < 256; });
    }
    return s.length < 40 && /^[0-9a-f:]+$/i.test(s) && s.split(":").length > 2;
  }
  function no() {
    if (v) { v.textContent = "none"; d.classList.add("none"); }
  }
  function go() {
    try {
      var c = new AbortController(), k = setTimeout(function () { c.abort(); }, 4000);
      fetch(u, { signal: c.signal, cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" })
        .then(function (r) { return r.ok ? r.text() : ""; })
        .then(function (t) { t = t.trim(); if (v && ok(t)) { v.textContent = t; if (b) b.hidden = false; } else no(); })
        .catch(no)
        .then(function () { clearTimeout(k); });
    } catch (e) { no(); }
  }
  document.readyState == "loading" ? document.addEventListener("DOMContentLoaded", go) : go();
})();
