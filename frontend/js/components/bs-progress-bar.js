import { BaseElement } from "./base-element.js";

/**
 * <bs-progress-bar value="64"> -- the amber bar used for card progress. It fills
 * forest green once complete, which is the same "confirmed" green used for a
 * finished night in the upload checklist.
 *
 * Attributes: value (0-100), size ("thin" | default), label (accessible name).
 */
class ProgressBar extends BaseElement {
  static observedAttributes = ["value", "size", "label"];

  render() {
    const value = Math.min(100, Math.max(0, Number(this.getAttribute("value")) || 0));
    const thin = this.getAttribute("size") === "thin";
    const complete = value >= 99.5;

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; }
        .track {
          height: ${thin ? "4px" : "14px"};
          background: var(--bs-track);
          ${thin ? "" : "border: 1px solid var(--bs-border);"}
        }
        .fill {
          height: 100%;
          width: ${value}%;
          background: ${complete ? "var(--bs-forest)" : "var(--bs-amber)"};
          transition: width 200ms linear;
        }
      </style>
      <div class="track" role="progressbar"
           aria-valuenow="${Math.round(value)}" aria-valuemin="0" aria-valuemax="100"
           aria-label="${this.getAttribute("label") ?? "Upload progress"}">
        <div class="fill"></div>
      </div>
    `;
  }
}

customElements.define("bs-progress-bar", ProgressBar);
