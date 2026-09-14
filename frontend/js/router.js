// A ~40-line history router. Routes are real paths, not hashes: the Go static
// handler falls back to index.html for extension-less paths, so /app/upload
// survives a reload.

const listeners = new Set();

/** The current path, without a trailing slash. "/" stays "/". */
export function path() {
  return location.pathname.replace(/\/+$/, "") || "/";
}

export function navigate(to, { replace = false } = {}) {
  if (to === path()) return;
  history[replace ? "replaceState" : "pushState"](null, "", to);
  announce();
}

export function onNavigate(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function announce() {
  for (const listener of listeners) listener(path());
}

window.addEventListener("popstate", announce);

// One delegated listener handles every in-app link, including links inside
// shadow roots: click events are composed, so they reach the document and
// composedPath() still shows the anchor that was hit.
document.addEventListener("click", (event) => {
  if (event.defaultPrevented || event.button !== 0) return;
  if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;

  const anchor = event.composedPath().find((node) => node.tagName === "A");
  if (!anchor || anchor.target === "_blank" || anchor.hasAttribute("download")) return;

  const url = new URL(anchor.href, location.href);
  if (url.origin !== location.origin) return;

  event.preventDefault();
  navigate(url.pathname);
});
