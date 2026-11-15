// The theme button: switches dark and light and remembers the choice. The / key goes to the lookup box.
document.getElementById("theme").onclick = function () {
  var r = document.documentElement,
    t = r.dataset.theme || (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  t = t == "dark" ? "light" : "dark";
  r.dataset.theme = t;
  bar(t);
  try { localStorage.theme = t; } catch (e) {}
};
addEventListener("keydown", function (e) {
  var f = document.querySelector(".lookup input");
  if (e.key == "/" && f && !e.ctrlKey && !e.metaKey && !e.altKey && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
    e.preventDefault();
    f.focus();
    f.select();
  }
});
