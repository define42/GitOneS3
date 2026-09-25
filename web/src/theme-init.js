// Runs as a same-origin, parser-blocking script before the body is painted.
// Keep this independent of React so saved themes also apply during loading.
(() => {
  if (window.gitoneTheme) return;

  const storageKey = "gitone.theme";
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const listeners = new Set();
  const normalize = (value) =>
    value === "light" || value === "dark" ? value : "system";
  let preference = "system";
  try {
    preference = normalize(window.localStorage.getItem(storageKey));
  } catch {
    // Private browsing or storage policies may prevent persistence.
  }

  function apply() {
    const theme = preference === "system"
      ? (media.matches ? "dark" : "light")
      : preference;
    document.documentElement.dataset.theme = theme;
    const browserColor = document.querySelector('meta[name="theme-color"]');
    if (browserColor) {
      browserColor.setAttribute("content", theme === "dark" ? "#0d1117" : "#24292f");
    }
    for (const listener of listeners) listener();
  }

  window.gitoneTheme = {
    getPreference: () => preference,
    setPreference(value) {
      preference = normalize(value);
      try {
        window.localStorage.setItem(storageKey, preference);
      } catch {
        // Switching still works for this page when saving is unavailable.
      }
      apply();
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };

  // These listeners belong to the document, not an individual React mount.
  media.addEventListener("change", () => {
    if (preference === "system") apply();
  });
  window.addEventListener("storage", (event) => {
    if (event.key !== storageKey && event.key !== null) return;
    try {
      if (event.storageArea !== window.localStorage) return;
      // Another tab may have saved again before this queued event is delivered.
      preference = normalize(window.localStorage.getItem(storageKey));
    } catch {
      return;
    }
    apply();
  });
  apply();
})();
