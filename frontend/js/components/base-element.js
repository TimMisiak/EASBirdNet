// Shared base class for Birdsense components.
//
// It exists to keep every component identical in shape: open shadow root, one
// render() that produces the whole subtree, and re-render when an observed
// attribute changes. Components that need finer-grained updates are free to
// extend HTMLElement directly instead -- this is a convenience, not a framework.

export class BaseElement extends HTMLElement {
  constructor() {
    super();
    this.attachShadow({ mode: "open" });
  }

  connectedCallback() {
    this.render();
  }

  attributeChangedCallback() {
    // Skip the pre-connect burst: connectedCallback renders once anyway.
    if (this.isConnected) this.render();
  }

  /** Subclasses override this and assign to this.shadowRoot.innerHTML. */
  render() {}

  /** Query inside this component's shadow root. */
  $(selector) {
    return this.shadowRoot.querySelector(selector);
  }
}

/**
 * Escape text for interpolation into an innerHTML template literal.
 * Every value that came from the API or from an attribute goes through this.
 */
export function escapeHTML(value) {
  return String(value ?? "").replace(
    /[&<>"']/g,
    (ch) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[ch],
  );
}
