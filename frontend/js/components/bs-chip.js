import { BaseElement, escapeHTML } from "./base-element.js";

/**
 * <bs-chip kind="done">Results sent</bs-chip> -- the status pill used in every
 * card table. `kind` is one of done, progress, processing, attention, admin,
 * neutral; anything else falls back to neutral.
 */
class Chip extends BaseElement {
  static observedAttributes = ["kind"];

  render() {
    const kind = this.getAttribute("kind") ?? "neutral";
    const label = this.textContent.trim();

    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: inline-block;
          font-size: 0.75rem;
          line-height: 1.5;
          padding: 0.1875rem 0.625rem;
          border-radius: var(--bs-radius-pill);
          border: 1px solid var(--bs-border-strong);
          background: var(--bs-surface);
          color: var(--bs-text-soft);
          white-space: nowrap;
        }
        :host([kind="done"]) {
          background: var(--bs-chip-done-bg);
          border-color: var(--bs-chip-done-border);
          color: var(--bs-chip-done-text);
        }
        :host([kind="progress"]) {
          background: var(--bs-chip-progress-bg);
          border-color: var(--bs-chip-progress-border);
          color: var(--bs-chip-progress-text);
        }
        :host([kind="processing"]) {
          background: var(--bs-chip-processing-bg);
          border-color: var(--bs-chip-processing-border);
          color: var(--bs-chip-processing-text);
        }
        :host([kind="attention"]) {
          background: var(--bs-chip-attention-bg);
          border-color: var(--bs-chip-attention-border);
          color: var(--bs-chip-attention-text);
        }
        :host([kind="admin"]) {
          background: var(--bs-forest);
          border-color: var(--bs-forest);
          color: var(--bs-on-forest);
        }
      </style>
      ${escapeHTML(label)}
    `;
    // kind is reflected by the caller as an attribute; keep it addressable for
    // the :host() rules above even when set before upgrade.
    if (!this.hasAttribute("kind")) this.setAttribute("kind", kind);
  }
}

customElements.define("bs-chip", Chip);
