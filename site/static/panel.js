// The copy buttons of the addresses; hidden until this runs. A browser without the clipboard API shows the failed state.
(function () {
  var all = document.querySelectorAll(".cp");
  for (var i = 0; i < all.length; i++) {
    // The other family's button waits for its address.
    if (!all[i].closest("[data-fam]")) all[i].hidden = false;
  }
  // The Headers button (the visitor's own page): copies the request headers, a JSON data block of the page.
  // The text is there already, so the write is called right inside the click.
  var hd = document.querySelector(".hd"), hj = document.getElementById("hdrs");
  if (hd && hj) {
    hd.hidden = false;
    var hl = hd.textContent;
    hd.onclick = function () {
      function flash(text) {
        clearTimeout(hd.k);
        hd.textContent = text;
        hd.k = setTimeout(function () { hd.textContent = hl; }, 1500);
      }
      (navigator.clipboard ? navigator.clipboard.writeText(hj.textContent) : Promise.reject()).then(function () { flash("Copied"); }, function () { flash("Copy failed"); });
    };
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest(".cp") : null;
    if (!b) return;
    var v = b.parentNode.querySelector(".v,pre"), t = v ? v.textContent : "";
    if (!v || (v.tagName != "PRE" && v.tagName != "A")) {
      t = t.trim();
      if (!/^[0-9a-fA-F:.]{2,45}$/.test(t)) return;
    }
    // The check mark (or the failed state) for 1.5 s.
    function show(ok) {
      clearTimeout(b.k);
      b.classList.toggle("ok", ok);
      b.setAttribute("aria-label", ok ? "Copied" : "Copy failed");
      b.k = setTimeout(function () { b.classList.remove("ok"); b.setAttribute("aria-label", "Copy"); }, 1500);
    }
    (navigator.clipboard ? navigator.clipboard.writeText(t) : Promise.reject()).then(function () { show(true); }, function () { show(false); });
  });
})();
