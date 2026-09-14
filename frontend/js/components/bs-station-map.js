import { escapeHTML } from "./base-element.js";
import { reset } from "../shared-styles.js";

/**
 * <bs-station-map> places recorders on an OpenStreetMap slippy map (Leaflet).
 *
 * Clicking the map or dragging the draft pin raises `bs-place` with the
 * coordinates; the fields next to the map stay the authoritative way to type
 * them. Set `stations` (the recorders in the field) and `draft` (the one being
 * added, or null) as properties.
 *
 * This extends HTMLElement rather than BaseElement: Leaflet owns the DOM inside
 * the map, so the shadow root is built once and pins are updated in place
 * instead of re-rendering.
 *
 * Leaflet is loaded from a CDN, not from this repo: the "leaflet" import map
 * entry in index.html pins the version and its hash. It is imported lazily, so
 * with no outbound network this element shows a note and the rest of the app
 * still works. Tiles come from OpenStreetMap's public servers, whose usage
 * policy (https://operations.osmfoundation.org/policies/tiles/) is fine with a
 * handful of coordinators but requires the attribution below to stay visible.
 */

// The CSS has to be linked inside the shadow root; document styles stop at the
// boundary. Keep the version in step with the import map in index.html.
const LEAFLET_CSS = "https://cdn.jsdelivr.net/npm/leaflet@1.9.4/dist/leaflet.css";
const LEAFLET_CSS_INTEGRITY = "sha384-sHL9NAb7lN7rfvG5lfHpm643Xkcjzp4jFvuavGOndn6pjVqS6ny56CAt3nsEVT4H";

const TILES = "https://tile.openstreetmap.org/{z}/{x}/{y}.png";
const ATTRIBUTION = '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors';

/** Where the map opens before there are any stations to fit. */
const EASTSIDE = [
  [47.52, -122.28],
  [47.74, -121.94],
];

let leaflet = null;
const loadLeaflet = () =>
  (leaflet ??= import("leaflet").catch((error) => {
    leaflet = null; // let the next map on the page try again
    throw error;
  }));

class StationMap extends HTMLElement {
  #stations = [];
  #draft = null;

  #L = null;
  #map = null;
  #started = false;
  #stationLayer = null;
  #draftMarker = null;
  #draggingDraft = false;
  #fitted = false;
  #resize = new ResizeObserver(() => this.#map?.invalidateSize());

  constructor() {
    super();
    this.attachShadow({ mode: "open" });
    this.shadowRoot.adoptedStyleSheets = [reset];
    this.shadowRoot.innerHTML = `
      <link rel="stylesheet" href="${LEAFLET_CSS}" integrity="${LEAFLET_CSS_INTEGRITY}" crossorigin="" />
      <style>
        :host { display: block; }
        .frame {
          position: relative;
          aspect-ratio: 4 / 3;
          border: 1px solid var(--bs-border);
          background: var(--bs-surface-sunk);
          overflow: hidden;
        }
        .map {
          position: absolute;
          inset: 0;
          font-family: var(--bs-font);
          background: var(--bs-surface-sunk);
        }
        .map.leaflet-grab { cursor: crosshair; }
        .map.leaflet-dragging .leaflet-grab { cursor: move; }
        .pin {
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
        }
        .pin--draft {
          background: var(--bs-forest);
          border-color: var(--bs-forest-edge);
          color: var(--bs-on-forest);
          box-shadow: 0 0 0 6px rgba(35, 64, 47, 0.16);
          cursor: grab;
        }
        .hint, .fallback {
          position: absolute;
          left: 14px;
          bottom: 14px;
          z-index: 1000;
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
          background: var(--bs-bg);
          border: 1px solid var(--bs-border);
          padding: 0.375rem 0.625rem;
          color: var(--bs-text-body);
          pointer-events: none;
        }
        .fallback { right: 14px; bottom: auto; top: 14px; }
        .leaflet-control-attribution { font-size: 0.625rem; }
      </style>
      <div class="frame">
        <div class="map"></div>
        <div class="hint">click to place a recorder</div>
        <p class="fallback" hidden>
          The map couldn't load, probably no connection to the map servers.
          Coordinates can still be typed beside it.
        </p>
      </div>
    `;
  }

  set stations(value) {
    this.#stations = value ?? [];
    this.#syncStations();
  }

  set draft(value) {
    this.#draft = value;
    this.#syncDraft();
  }

  connectedCallback() {
    // Observing the element (not the window) also catches the page's grid
    // collapsing to one column, and a parent moving this element between
    // renders -- Leaflet measures its container and needs telling.
    this.#resize.observe(this);
    // The parent moves this element between renders, so it can connect again
    // before Leaflet has even loaded; only the first connection starts it.
    if (!this.#started) {
      this.#started = true;
      this.#init();
    }
  }

  disconnectedCallback() {
    this.#resize.disconnect();
  }

  async #init() {
    try {
      this.#L = await loadLeaflet();
    } catch {
      this.shadowRoot.querySelector(".hint").hidden = true;
      this.shadowRoot.querySelector(".fallback").hidden = false;
      return;
    }
    const L = this.#L;

    // trackResize off: the ResizeObserver does that job, and a window listener
    // would keep the map alive after this element is gone.
    this.#map = L.map(this.shadowRoot.querySelector(".map"), { trackResize: false });
    L.tileLayer(TILES, { maxZoom: 19, attribution: ATTRIBUTION }).addTo(this.#map);
    this.#map.fitBounds(EASTSIDE);
    this.#stationLayer = L.layerGroup().addTo(this.#map);

    this.#map.on("click", (event) => this.#place(event.latlng));

    this.#syncStations();
    this.#syncDraft();
  }

  #place({ lat, lng }) {
    this.dispatchEvent(
      new CustomEvent("bs-place", {
        detail: { latitude: round(lat), longitude: round(wrapLongitude(lng)) },
      }),
    );
  }

  #syncStations() {
    if (!this.#map) return;
    const L = this.#L;
    this.#stationLayer.clearLayers();
    this.#stations.forEach((station, i) => {
      L.marker([station.latitude, station.longitude], {
        icon: pinIcon(L, i + 1),
        keyboard: false,
      })
        // Tooltip strings are set as innerHTML.
        .bindTooltip(`${escapeHTML(station.name)} · ${escapeHTML(station.id)}`)
        .addTo(this.#stationLayer);
    });

    // Frame the stations once; after that the view is the coordinator's.
    if (!this.#fitted && this.#stations.length) {
      this.#fitted = true;
      const bounds = L.latLngBounds(this.#stations.map((s) => [s.latitude, s.longitude]));
      this.#map.fitBounds(bounds, { padding: [40, 40], maxZoom: 14 });
    }
  }

  #syncDraft() {
    if (!this.#map) return;
    const L = this.#L;

    if (!this.#draft) {
      this.#draftMarker?.remove();
      this.#draftMarker = null;
      return;
    }

    const latlng = [this.#draft.latitude, this.#draft.longitude];
    const label = escapeHTML(this.#draft.name || "New recorder");

    if (!this.#draftMarker) {
      this.#draftMarker = L.marker(latlng, {
        icon: pinIcon(L, "+", true),
        draggable: true,
        zIndexOffset: 1000,
      })
        .bindTooltip(label)
        .on("dragstart", () => (this.#draggingDraft = true))
        .on("drag", (event) => this.#place(event.target.getLatLng()))
        .on("dragend", (event) => {
          this.#draggingDraft = false;
          this.#place(event.target.getLatLng());
        })
        .addTo(this.#map);
    } else if (!this.#draggingDraft) {
      // Mid-drag the marker already knows where it is; the rounded echo from
      // the fields would only make it jitter.
      this.#draftMarker.setLatLng(latlng);
    }
    this.#draftMarker.setTooltipContent(label);

    // A pasted GPS reading may be off screen; follow it.
    if (!this.#draggingDraft && !this.#map.getBounds().contains(latlng)) {
      this.#map.panTo(latlng);
    }
  }
}

function pinIcon(L, label, draft = false) {
  return L.divIcon({
    className: draft ? "pin pin--draft" : "pin",
    html: escapeHTML(label),
    iconSize: [26, 26],
  });
}

/** Leaflet lets you pan past the antimeridian; stored longitudes shouldn't. */
const wrapLongitude = (lng) => ((((lng + 180) % 360) + 360) % 360) - 180;
const round = (n) => Number(n.toFixed(5));

customElements.define("bs-station-map", StationMap);
