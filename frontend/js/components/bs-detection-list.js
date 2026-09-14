import { BaseElement, escapeHTML } from "./base-element.js";
import { fetchDetections } from "../api.js";

/**
 * <bs-detection-list> loads recent detections and renders one
 * <bs-detection-card> per result. It owns the loading and error states so the
 * card component can stay a pure presentation element.
 */
class DetectionList extends BaseElement {
  #state = { status: "loading", detections: [], error: null };

  connectedCallback() {
    this.render();
    this.#load();
  }

  async #load() {
    try {
      const detections = await fetchDetections();
      this.#state = { status: "ready", detections, error: null };
    } catch (err) {
      this.#state = { status: "error", detections: [], error: err };
    }
    if (this.isConnected) this.render();
  }

  render() {
    const { status, detections, error } = this.#state;

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; }
        h2 { margin: 0 0 var(--bs-space-3); font-size: 1.05rem; }
        ul { list-style: none; margin: 0; padding: 0; display: grid; gap: var(--bs-space-2); }
        .note { color: var(--bs-text-muted); }
        .error { color: var(--bs-text); }
      </style>
      <h2>Recent detections</h2>
      ${this.#body(status, detections, error)}
    `;

    if (status === "ready") {
      const list = this.$("ul");
      for (const detection of detections) {
        const item = document.createElement("li");
        const card = document.createElement("bs-detection-card");
        // Rich data goes in as a property, not an attribute: attributes are
        // strings, and stringifying an object here would only cost a parse.
        card.detection = detection;
        item.append(card);
        list.append(item);
      }
    }
  }

  #body(status, detections, error) {
    if (status === "loading") return `<p class="note">Listening…</p>`;
    if (status === "error") {
      return `<p class="error">Could not load detections: ${escapeHTML(error.message)}</p>`;
    }
    if (detections.length === 0) return `<p class="note">Nothing heard yet today.</p>`;
    return `<ul></ul>`;
  }
}

customElements.define("bs-detection-list", DetectionList);
