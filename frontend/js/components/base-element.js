// Shared base class for Birdsense components.
//
// It exists to keep every component identical in shape: open shadow root, one
// render() that produces the whole subtree, and re-render when an observed
// attribute changes. Components that need finer-grained updates are free to
// extend HTMLElement directly instead -- this is a convenience, not a framework.

import { reset } from "../shared-styles.js";

export class BaseElement extends HTMLElement {
  /**
   * Shared stylesheets to adopt into the shadow root, as an array of
   * CSSStyleSheet (see shared-styles.js). Component-specific rules still go in
   * the component's own <style>; this is only for the primitives -- buttons,
   * tables, form fields -- that would otherwise be copied into a dozen files.
   */
  static styles = [];

  constructor() {
    super();
    this.attachShadow({ mode: "open" });
    this.shadowRoot.adoptedStyleSheets = [reset, ...this.constructor.styles];
  }

  connectedCallback() {
    this.#wire();
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

  /** Query all, as a real array. */
  $$(selector) {
    return [...this.shadowRoot.querySelectorAll(selector)];
  }

  /**
   * Handlers for `data-action`, `data-change` and `data-submit` attributes in
   * the template, keyed by name. Subclasses override this getter.
   *
   * render() replaces the whole subtree, so per-element listeners would have to
   * be re-attached every time; these are delegated from the shadow root once
   * and survive re-renders. The handler is called with (element, event).
   */
  get actions() {
    return {};
  }

  #wire() {
    if (this.#wired) return;
    this.#wired = true;
    const dispatch = (attr) => (event) => {
      const el = event.target?.closest?.(`[${attr}]`);
      if (!el || !this.shadowRoot.contains(el)) return;
      const handler = this.actions[el.getAttribute(attr)];
      if (!handler) return;
      if (event.type !== "input") event.preventDefault();
      handler.call(this, el, event);
    };
    this.shadowRoot.addEventListener("click", dispatch("data-action"));
    this.shadowRoot.addEventListener("change", dispatch("data-change"));
    this.shadowRoot.addEventListener("input", dispatch("data-input"));
    this.shadowRoot.addEventListener("submit", dispatch("data-submit"));
  }

  #wired = false;
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
