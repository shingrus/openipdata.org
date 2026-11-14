// Applies the saved theme before the first paint.
try {
  var t = localStorage.theme;
  if (t == "dark" || t == "light") document.documentElement.dataset.theme = t;
} catch (e) {}
