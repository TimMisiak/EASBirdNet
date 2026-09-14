import { BaseElement, escapeHTML } from "./base-element.js";

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

function timeAgo(isoString) {
  const then = new Date(isoString);
  if (Number.isNaN(then.getTime())) return "";
  const minutes = Math.round((then.getTime() - Date.now()) / 60000);
  if (Math.abs(minutes) < 60) return relative.format(minutes, "minute");
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 24) return relative.format(hours, "hour");
  return relative.format(Math.round(hours / 24), "day");
}

/**
 * <bs-detection-card> renders one detection. Set the `detection` property with
 * the object from the API; the card renders whatever it is handed and holds no
 * state of its own.
 */
class DetectionCard extends BaseElement {
  #detection = null;

  set detection(value) {
    this.#detection = value;
    if (this.isConnected) this.render();
  }

  get detection() {
    return this.#detection;
  }

  render() {
    const d = this.#detection;
    if (!d) {
      this.shadowRoot.innerHTML = "";
      return;
    }

    const confidence = Math.round((d.confidence ?? 0) * 100);

    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: block;
          background: var(--bs-surface);
          border: 1px solid var(--bs-border);
          border-radius: var(--bs-radius);
          padding: var(--bs-space-3);
        }
        .top { display: flex; justify-content: space-between; gap: var(--bs-space-3); align-items: baseline; }
        .name { font-weight: 600; }
        .latin { font-style: italic; color: var(--bs-text-muted); font-size: 0.9rem; }
        .confidence {
          background: var(--bs-accent-soft);
          color: var(--bs-accent);
          border-radius: 999px;
          padding: var(--bs-space-1) var(--bs-space-2);
          font-size: 0.8rem;
          font-variant-numeric: tabular-nums;
          white-space: nowrap;
        }
        .meta { color: var(--bs-text-muted); font-size: 0.85rem; margin-top: var(--bs-space-1); }
      </style>
      <div class="top">
        <div>
          <div class="name">${escapeHTML(d.commonName)}</div>
          <div class="latin">${escapeHTML(d.scientificName)}</div>
        </div>
        <div class="confidence">${confidence}% match</div>
      </div>
      <div class="meta">
        ${escapeHTML(d.station)} ·
        <time datetime="${escapeHTML(d.detectedAt)}">${escapeHTML(timeAgo(d.detectedAt))}</time>
      </div>
    `;
  }
}

customElements.define("bs-detection-card", DetectionCard);
