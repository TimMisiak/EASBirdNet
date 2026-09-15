import { BaseElement, escapeHTML } from "./base-element.js";
import * as flow from "../upload-flow.js";

/**
 * <bs-upload-steps step="2"> -- the row atop the Upload tab during the card
 * upload, showing where in the four steps the volunteer is and which card this
 * is. It reads the reference from the flow so the volunteer always has the
 * string they'd quote in an email in front of them.
 */
const STEPS = ["Details", "Check card", "Upload", "Done"];

class UploadSteps extends BaseElement {
  static observedAttributes = ["step"];

  #unsubscribe = null;

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = flow.subscribe(() => this.render());
    flow.current().catch(() => {});
  }

  disconnectedCallback() {
    this.#unsubscribe?.();
  }

  render() {
    const current = Number(this.getAttribute("step")) || 1;
    const reference = flow.get().upload?.reference ?? "";

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; margin: calc(-1 * var(--bs-space-3)) 0 var(--bs-space-6); }
        .band {
          list-style: none;
          margin: 0;
          padding: 0;
          display: flex;
          align-items: center;
          gap: var(--bs-space-2);
          flex-wrap: wrap;
        }
        .step {
          border: 1px solid var(--bs-border-strong);
          border-radius: var(--bs-radius-pill);
          padding: 0.3125rem 0.875rem;
          font-size: 0.78125rem;
          color: var(--bs-text-muted);
        }
        .step[data-state="current"] {
          background: var(--bs-text);
          border-color: var(--bs-text);
          color: var(--bs-on-forest);
        }
        .step[data-state="past"] { color: var(--bs-text-soft); }
        .reference {
          margin-left: auto;
          font-family: var(--bs-font-mono);
          font-size: 0.71875rem;
          color: var(--bs-text-muted);
        }
      </style>
      <ol class="band" aria-label="Upload steps">
        ${STEPS.map((label, i) => {
          const n = i + 1;
          const state = n === current ? "current" : n < current ? "past" : "future";
          return `<li class="step" data-state="${state}"
                      ${n === current ? 'aria-current="step"' : ""}>${n} ${label}</li>`;
        }).join("")}
        ${reference ? `<span class="reference">${escapeHTML(reference)}</span>` : ""}
      </ol>
    `;
  }
}

customElements.define("bs-upload-steps", UploadSteps);
