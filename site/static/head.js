// Applies the saved theme before the first paint.
// The colour of the phone's browser bar follows a theme chosen by hand (the --bg of each theme in style.css).
function bar(t) {
  document.querySelectorAll("meta[name=theme-color]").forEach(function (m) { m.content = t == "dark" ? "#0a0c0f" : "#f3f4f2"; });
}
try {
  var t = localStorage.theme;
  if (t == "dark" || t == "light") { document.documentElement.dataset.theme = t; bar(t); }
} catch (e) {}
