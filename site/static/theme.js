// The theme button: switches dark and light and remembers the choice.
document.getElementById("theme").onclick = function () {
  var r = document.documentElement,
    t = r.dataset.theme || (matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  t = t == "dark" ? "light" : "dark";
  r.dataset.theme = t;
  try { localStorage.theme = t; } catch (e) {}
};
