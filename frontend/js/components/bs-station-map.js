import { BaseElement, escapeHTML } from "./base-element.js";

/**
 * <bs-station-map> places recorders on a plan of the Eastside.
 *
 * It is deliberately not a slippy map: there is no tile provider dependency
 * here, and for five recorders inside one county the useful question is "is
 * this pin in roughly the right place relative to the others", which a fixed
 * extent answers. Clicking or dragging raises `bs-place` with real coordinates,
 * and the fields next to the map stay the authoritative way to type them.
 */

/** The extent the plan covers, with room around the current stations. */
const BOUNDS = { north: 47.74, south: 47.52, west: -122.28, east: -121.94 };

class StationMap extends BaseElement {
  #stations = [];
  #draft = null;
  #dragging = false;

  set stations(value) {
    this.#stations = value ?? [];
    if (this.isConnected) this.render();
  }

  set draft(value) {
    this.#draft = value;
    if (this.isConnected) this.render();
  }

  connectedCallback() {
    super.connectedCallback();
    // The listeners go on the host, not on .plan: render() replaces the whole
    // shadow subtree whenever a pin moves, and anything bound to .plan would be
    // thrown away with it mid-drag.
    this.addEventListener("pointerdown", (event) => {
      this.#dragging = true;
      this.setPointerCapture(event.pointerId);
      this.#place(event);
    });
    this.addEventListener("pointermove", (event) => {
      if (this.#dragging) this.#place(event);
    });
    this.addEventListener("pointerup", () => (this.#dragging = false));
    this.addEventListener("pointercancel", () => (this.#dragging = false));
  }

  #place(event) {
    const box = this.$(".plan").getBoundingClientRect();
    const x = clamp((event.clientX - box.left) / box.width);
    const y = clamp((event.clientY - box.top) / box.height);
    this.dispatchEvent(
      new CustomEvent("bs-place", {
        detail: {
          latitude: round(BOUNDS.north - y * (BOUNDS.north - BOUNDS.south)),
          longitude: round(BOUNDS.west + x * (BOUNDS.east - BOUNDS.west)),
        },
      }),
    );
  }

  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; }
        .plan {
          position: relative;
          aspect-ratio: 4 / 3;
          border: 1px solid var(--bs-border);
          background:
            repeating-linear-gradient(90deg, #e9e7dc 0 1px, transparent 1px 34px),
            repeating-linear-gradient(0deg, #e9e7dc 0 1px, #f2f0e7 1px 34px);
          overflow: hidden;
          cursor: crosshair;
          touch-action: none;
        }
        /* Two washes standing in for the big green spaces the stations sit in. */
        .relief {
          position: absolute;
          inset: 0;
          background:
            radial-gradient(circle at 34% 58%, rgba(62, 107, 79, 0.16) 0 22%, transparent 55%),
            radial-gradient(circle at 72% 30%, rgba(62, 107, 79, 0.13) 0 18%, transparent 50%);
          pointer-events: none;
        }
        .pin {
          position: absolute;
          transform: translate(-50%, -50%);
          width: 26px;
          height: 26px;
          border-radius: 50%;
          background: var(--bs-amber);
          border: 2px solid var(--bs-amber-edge);
          color: var(--bs-text);
          display: flex;
          align-items: center;
          justify-content: center;
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
          pointer-events: none;
        }
        .pin[data-draft="true"] {
          background: var(--bs-forest);
          border-color: var(--bs-forest-edge);
          color: var(--bs-on-forest);
          box-shadow: 0 0 0 6px rgba(35, 64, 47, 0.16);
        }
        .hint {
          position: absolute;
          left: 14px;
          bottom: 14px;
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
          background: var(--bs-bg);
          border: 1px solid var(--bs-border);
          padding: 0.375rem 0.625rem;
          color: var(--bs-text-body);
          pointer-events: none;
        }
      </style>
      <div class="plan">
        <div class="relief"></div>
        ${this.#stations.map((station, i) => pin(station, i + 1)).join("")}
        ${this.#draft ? pin(this.#draft, "+", true) : ""}
        <div class="hint">map — click to place a recorder</div>
      </div>
    `;
  }
}

function pin(station, label, draft = false) {
  const x = ((station.longitude - BOUNDS.west) / (BOUNDS.east - BOUNDS.west)) * 100;
  const y = ((BOUNDS.north - station.latitude) / (BOUNDS.north - BOUNDS.south)) * 100;
  return `
    <div class="pin" data-draft="${draft}"
         style="left: ${clamp(x / 100) * 100}%; top: ${clamp(y / 100) * 100}%;"
         title="${escapeHTML(station.name ?? "New recorder")}">${label}</div>
  `;
}

const clamp = (n) => Math.min(1, Math.max(0, n));
const round = (n) => Number(n.toFixed(5));

customElements.define("bs-station-map", StationMap);
