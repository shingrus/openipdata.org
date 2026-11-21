// The theme button: switches dark and light and remembers the choice. The / key goes to the lookup box.
// Ctrl+Enter (Cmd+Enter) in a text area sends its form with the default button (Look up on the bulk page).
document.getElementById("theme").onclick = function () {
  var r = document.documentElement,
    t = r.dataset.theme || (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  t = t == "dark" ? "light" : "dark";
  r.dataset.theme = t;
  bar(t);
  try { localStorage.theme = t; } catch (e) {}
};
addEventListener("keydown", function (e) {
  var f = document.querySelector(".lookup input"), t = e.target;
  if (e.key == "Enter" && (e.ctrlKey || e.metaKey) && t.tagName == "TEXTAREA" && t.form) {
    e.preventDefault();
    if (t.form.requestSubmit) t.form.requestSubmit(); else t.form.submit();
    return;
  }
  if (e.key == "/" && f && !e.ctrlKey && !e.metaKey && !e.altKey && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
    e.preventDefault();
    f.focus();
    f.select();
  }
});
