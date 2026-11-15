// The copy buttons of the addresses; hidden until this runs. A browser without the clipboard API shows the failed state.
(function () {
  var all = document.querySelectorAll(".cp");
  for (var i = 0; i < all.length; i++) {
    // The other family's button waits for its address.
    if (!all[i].closest("[data-fam]")) all[i].hidden = false;
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest(".cp") : null;
    if (!b) return;
    var v = b.parentNode.querySelector(".v,pre"), t = v ? v.textContent : "";
    if (!v || v.tagName != "PRE") {
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
