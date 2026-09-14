import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, typography } from "../shared-styles.js";
import * as api from "../api.js";
import "./bs-station-map.js";

/**
 * <bs-admin-recorders> -- where the recorders are, and how a new one gets on
 * the list. The map and the coordinate fields are two views of the same two
 * numbers: clicking the map fills the fields, and typing in the fields moves
 * the pin, because a coordinator pasting a GPS reading shouldn't have to aim.
 */
class AdminRecorders extends BaseElement {
  static styles = [typography, controls, forms, panels];

  #state = { status: "loading", stations: [], error: null };
  #draft = { id: "", name: "", latitude: "", longitude: "" };
  #formError = null;
  #busy = false;

  connectedCallback() {
    super.connectedCallback();
    this.#load().then(() => this.render());
  }

  get actions() {
    return {
      field: (el) => {
        this.#draft[el.name] = el.value;
        this.#syncMap();
      },
      add: async (el, event) => {
        event.preventDefault();
        if (this.#busy) return;
        this.#busy = true;
        this.#formError = null;
        this.render();
        try {
          await api.addStation({
            id: this.#draft.id,
            name: this.#draft.name,
            latitude: Number(this.#draft.latitude),
            longitude: Number(this.#draft.longitude),
          });
          this.#draft = { id: "", name: "", latitude: "", longitude: "" };
          await this.#load();
        } catch (error) {
          this.#formError = error;
        } finally {
          this.#busy = false;
          if (this.isConnected) this.render();
        }
      },
    };
  }

  async #load() {
    try {
      const { stations } = await api.fetchStations();
      this.#state = { status: "ready", stations, error: null };
    } catch (error) {
      this.#state = { status: "error", stations: [], error };
    }
  }

  /** The draft pin only exists once both coordinates parse. */
  #draftPin() {
    const latitude = Number(this.#draft.latitude);
    const longitude = Number(this.#draft.longitude);
    if (!this.#draft.latitude || !this.#draft.longitude) return null;
    if (Number.isNaN(latitude) || Number.isNaN(longitude)) return null;
    return { latitude, longitude, name: this.#draft.name || "New recorder" };
  }

  #syncMap() {
    const map = this.$("bs-station-map");
    if (map) map.draft = this.#draftPin();
  }

  render() {
    const { status, stations, error } = this.#state;

    this.shadowRoot.innerHTML = `
      <style>
        .columns {
          display: grid;
          grid-template-columns: minmax(0, 1.25fr) minmax(0, 0.75fr);
          gap: 2.25rem;
          align-items: start;
        }
        .aside { display: flex; flex-direction: column; gap: 1.125rem; }
        .coords { display: grid; grid-template-columns: 1fr 1fr; gap: 0.625rem; margin-bottom: var(--bs-space-5); }
        .coord-head { display: flex; align-items: center; gap: 0.625rem; margin-bottom: 0.625rem; }
        .station {
          border-top: 1px solid var(--bs-border);
          padding: var(--bs-space-3) 0;
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: var(--bs-space-3);
        }
        .station-name { font-size: 0.90625rem; }
        .station-meta { font-family: var(--bs-font-mono); font-size: 0.71875rem; color: var(--bs-text-muted); margin-top: 0.1875rem; }
        .error { margin-bottom: var(--bs-space-4); }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      <div class="columns">
        <div>
          <bs-station-map></bs-station-map>
          <p class="note" style="margin-top: var(--bs-space-3);">
            Click anywhere on the map to drop a pin, or drag it to correct the position.
            Coordinates fill in beside it and can be typed instead.
          </p>
          ${status === "error" ? `<p class="note">Couldn't load the recorders: ${escapeHTML(error.message)}</p>` : ""}
        </div>

        <div class="aside">
          <form class="panel" data-submit="add">
            <h3 style="margin-bottom: 1.125rem;">New recorder</h3>

            <label class="label" for="station-name">Station name</label>
            <input class="field field--sunk" id="station-name" name="name" type="text" required
                   value="${escapeHTML(this.#draft.name)}" placeholder="Marymoor Park – Snag Row"
                   data-change="field" data-input="field" style="margin-bottom: var(--bs-space-4);" />

            <label class="label" for="station-id">Recorder ID</label>
            <input class="field field--sunk field--mono" id="station-id" name="id" type="text"
                   value="${escapeHTML(this.#draft.id)}" placeholder="SW-06"
                   data-change="field" data-input="field" style="margin-bottom: 1.125rem;" />

            <div class="coord-head">
              <span class="label" style="margin: 0;">Location</span>
              <span class="tag">set from the map</span>
            </div>
            <div class="coords">
              <input class="field field--sunk field--mono" name="latitude" type="text" inputmode="decimal"
                     aria-label="Latitude" placeholder="47.66021"
                     value="${escapeHTML(this.#draft.latitude)}" data-change="field" data-input="field" />
              <input class="field field--sunk field--mono" name="longitude" type="text" inputmode="decimal"
                     aria-label="Longitude" placeholder="-122.11384"
                     value="${escapeHTML(this.#draft.longitude)}" data-change="field" data-input="field" />
            </div>

            ${this.#formError ? `<p class="error">${escapeHTML(this.#formError.message)}</p>` : ""}
            <button class="btn btn--forest btn--small btn--block" ${this.#busy ? "disabled" : ""}>
              ${this.#busy ? "Adding…" : "Add recorder"}
            </button>
          </form>

          <div>
            <div class="eyebrow" style="margin-bottom: var(--bs-space-1);">In the field</div>
            ${stations
              .map(
                (s) => `
              <div class="station">
                <div>
                  <div class="station-name">${escapeHTML(s.name)}</div>
                  <div class="station-meta">${escapeHTML(s.id)} · ${s.latitude}, ${s.longitude}</div>
                </div>
              </div>`,
              )
              .join("")}
          </div>
        </div>
      </div>
    `;

    const map = this.$("bs-station-map");
    map.stations = stations;
    map.draft = this.#draftPin();
    map.addEventListener("bs-place", (event) => {
      this.#draft.latitude = String(event.detail.latitude);
      this.#draft.longitude = String(event.detail.longitude);
      // Only the two coordinate fields change; re-rendering the form here would
      // steal focus from the name field mid-typing.
      this.$('[name="latitude"]').value = this.#draft.latitude;
      this.$('[name="longitude"]').value = this.#draft.longitude;
      this.#syncMap();
    });
  }
}

customElements.define("bs-admin-recorders", AdminRecorders);
