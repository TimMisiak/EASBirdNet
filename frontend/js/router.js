// A ~40-line history router. Routes are real paths, not hashes: the Go static
// handler falls back to index.html for extension-less paths, so /app/upload
// survives a reload.

const listeners = new Set();

/** The current path, without a trailing slash. "/" stays "/". */
export function path() {
  return location.pathname.replace(/\/+$/, "") || "/";
}

/** Go to a path, with a query string if it has one. */
export function navigate(to, { replace = false } = {}) {
  const url = new URL(to, location.href);
  if (url.pathname.replace(/\/+$/, "") === location.pathname.replace(/\/+$/, "") && url.search === location.search) return;
  history[replace ? "replaceState" : "pushState"](null, "", url.pathname + url.search);
  announce();
}

/** The current query string's parameters. */
export function query() {
  return new URLSearchParams(location.search);
}

/**
 * Rewrite the query string in place, without a history entry or telling
 * anyone: for a page keeping its own filters in the URL, so a reload or a
 * link lands on the same view, while the page stays as it is.
 */
export function replaceQuery(params) {
  const search = new URLSearchParams(params).toString();
  history.replaceState(null, "", `${location.pathname}${search ? `?${search}` : ""}`);
}

export function onNavigate(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function announce() {
  // Drawing a path is what swaps the elements that listen for it, so the set
  // changes under this loop: a snapshot keeps the ones that subscribed
  // part-way through out (they are already drawing this path), and the
  // membership check keeps the ones that have just been torn down out. Either
  // one left in draws the same path a second time.
  const at = path();
  for (const listener of [...listeners]) {
    if (listeners.has(listener)) listener(at);
  }
}

// Every route change starts at the top of the page (<bs-app> does the
// scrolling, once it has drawn the new page). Left on "auto" the browser would
// also try to restore a scroll offset on Back, against a page that hasn't
// fetched its rows yet and is the wrong height -- two answers, neither of them
// where the reader was. One deterministic answer is worth more than a restore
// that lands short.
history.scrollRestoration = "manual";

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
  navigate(url.pathname + url.search);
});
