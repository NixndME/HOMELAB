// Runs in <head> before first paint so the page never flashes the wrong theme.
(function () {
  var saved = null;
  try { saved = localStorage.getItem('cp-theme'); } catch (e) { /* storage blocked: follow the OS */ }
  var dark = saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
})();
