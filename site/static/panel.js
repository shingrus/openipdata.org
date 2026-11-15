// The copy buttons of the addresses and the Share button; hidden until this runs. Fetches nothing.
(function () {
  function copy(t) {
    if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(t);
    return new Promise(function (ok, no) {
      var a = document.createElement("textarea");
      a.value = t; a.readOnly = true; a.style.cssText = "position:fixed;top:0;opacity:0";
      document.body.appendChild(a); a.select();
      var r = false;
      try { r = document.execCommand("copy"); } catch (e) {}
      document.body.removeChild(a);
      r ? ok() : no();
    });
  }
  // A short state on the button: the check mark (copy buttons) or the new label (Share), for 1.5 s.
  function flash(b, ok, text) {
    var c = b.classList.contains("cp");
    clearTimeout(b.k);
    if (c) { b.classList.toggle("ok", ok); b.setAttribute("aria-label", ok ? "Copied" : "Copy failed"); }
    else b.textContent = text;
    b.k = setTimeout(function () {
      if (c) { b.classList.remove("ok"); b.setAttribute("aria-label", "Copy"); }
      else b.textContent = b.dataset.t;
    }, 1500);
  }
  var all = document.querySelectorAll(".cp,.sh");
  for (var i = 0; i < all.length; i++) {
    all[i].dataset.t = all[i].textContent;
    if (!all[i].closest("[data-fam]")) all[i].hidden = false; // the other family's button waits for its address
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest(".cp,.sh") : null;
    if (!b) return;
    var c = b.classList.contains("cp"), t = "";
    if (c) {
      var v = b.parentNode.querySelector(".v,pre");
      t = v ? v.textContent : "";
      if (!v || v.tagName != "PRE") {
        t = t.trim();
        if (!/^[0-9a-fA-F:.]{2,45}$/.test(t)) return;
      }
    } else {
      t = b.dataset.url || "";
      if (!/^https?:\/\/[^\s]{1,300}$/.test(t)) return;
      b.textContent = "Copying...";
    }
    copy(t).then(function () {
      flash(b, true, "Link copied");
    }, function () {
      flash(b, false, "Copy failed");
    });
  });
})();
