import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, typography } from "../shared-styles.js";
import * as api from "../api.js";
import "./bs-station-map.js";

/**
 * <bs-admin-recorders> -- where the recorders are, and how one gets on the
 * list or changes. The map and the coordinate fields are two views of the same
 * two numbers: clicking the map fills the fields, and typing in the fields
 * moves the pin, because a coordinator pasting a GPS reading shouldn't have to
 * aim.
 *
 * The form has two modes. With nothing selected it adds a recorder. Choosing
 * one from the list (or its pin) loads it into the same form to change or
 * delete, and "Add new…" at the foot of the list goes back. The recorder ID is
 * printed on the unit, so an edit shows it but can't change it.
 */

const EMPTY = { id: "", name: "", latitude: "", longitude: "" };

const draftOf = (station) => ({
  id: station.id,
  name: station.name,
  latitude: String(station.latitude),
  longitude: String(station.longitude),
});

class AdminRecorders extends BaseElement {
  static styles = [typography, controls, forms, panels];

  #state = { status: "loading", stations: [], error: null };
  #draft = { ...EMPTY };
  /** The id of the recorder loaded into the form, or null when adding. */
  #selected = null;
  #confirmingDelete = false;
  #saved = false;
  #formError = null;
  #busy = false;

  // One map for the life of the page, moved into each fresh render: rebuilding
  // it would reload the tiles and throw away wherever the coordinator had
  // panned and zoomed to.
  #map = document.createElement("bs-station-map");

  constructor() {
    super();
    this.#map.addEventListener("bs-place", (event) => {
      this.#draft.latitude = String(event.detail.latitude);
      this.#draft.longitude = String(event.detail.longitude);
      // Only the two coordinate fields change; re-rendering the form here would
      // steal focus from the name field mid-typing.
      this.$('[name="latitude"]').value = this.#draft.latitude;
      this.$('[name="longitude"]').value = this.#draft.longitude;
      this.#edited();
    });
    this.#map.addEventListener("bs-select", (event) => this.#select(event.detail.id));
  }

  connectedCallback() {
    super.connectedCallback();
    this.#load().then(() => this.render());
  }

  get actions() {
    return {
      field: (el) => {
        this.#draft[el.name] = el.value;
        this.#edited();
      },
      select: (el) => this.#select(el.dataset.id),
      addNew: () => this.#select(null),
      submit: async (el, event) => {
        event.preventDefault();
        if (this.#busy) return;
        const editing = this.#selected;
        const body = {
          name: this.#draft.name,
          latitude: Number(this.#draft.latitude),
          longitude: Number(this.#draft.longitude),
        };
        this.#busy = true;
        this.#formError = null;
        this.#saved = false;
        this.render();
        try {
          if (editing) {
            await api.updateStation(editing, body);
          } else {
            await api.addStation({ id: this.#draft.id, ...body });
          }
          await this.#load();
          if (editing) {
            // Show what the server kept (it trims the name), still selected.
            const station = this.#station(editing);
            if (station) this.#draft = draftOf(station);
            this.#saved = true;
          } else {
            this.#draft = { ...EMPTY };
          }
        } catch (error) {
          this.#formError = error;
        } finally {
          this.#busy = false;
          if (this.isConnected) this.render();
        }
      },
      askDelete: () => {
        this.#confirmingDelete = true;
        this.#formError = null;
        this.render();
        // Focus the safe choice, so an Enter doesn't delete anything.
        this.$('[data-action="keepRecorder"]')?.focus();
      },
      keepRecorder: () => {
        this.#confirmingDelete = false;
        this.render();
      },
      deleteRecorder: async () => {
        const id = this.#selected;
        if (!id || this.#busy) return;
        this.#busy = true;
        this.render();
        try {
          await api.removeStation(id);
          await this.#load();
          this.#selected = null;
          this.#draft = { ...EMPTY };
        } catch (error) {
          this.#formError = error;
        } finally {
          this.#confirmingDelete = false;
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

  #station(id) {
    return this.#state.stations.find((s) => s.id === id);
  }

  /** Load a recorder into the form, or pass null to go back to adding one. */
  #select(id) {
    const station = id ? this.#station(id) : null;
    // "Add new…" while already adding keeps whatever was half typed.
    if (station || this.#selected) {
      this.#selected = station?.id ?? null;
      this.#draft = station ? draftOf(station) : { ...EMPTY };
      this.#confirmingDelete = false;
      this.#formError = null;
      this.#saved = false;
      this.render();
    }
    this.$("#station-name")?.focus();
  }

  /** A field or the pin changed: the "saved" note no longer describes the form. */
  #edited() {
    if (this.#saved) {
      this.#saved = false;
      this.$(".saved")?.remove();
    }
    this.#syncMap();
  }

  /** The draft pin only exists once both coordinates parse. */
  #draftPin() {
    const latitude = Number(this.#draft.latitude);
    const longitude = Number(this.#draft.longitude);
    if (!this.#draft.latitude || !this.#draft.longitude) return null;
    if (Number.isNaN(latitude) || Number.isNaN(longitude)) return null;
    return { latitude, longitude, name: this.#draft.name || this.#draft.id || "New recorder" };
  }

  #syncMap() {
    this.#map.draft = this.#draftPin();
  }

  render() {
    const { status, stations, error } = this.#state;
    const editing = this.#selected;

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
        .readout { margin-bottom: 1.125rem; }
        .readout .tag { margin-left: auto; }
        .station {
          display: block;
          width: 100%;
          text-align: left;
          background: none;
          border: 0;
          border-top: 1px solid var(--bs-border);
          border-radius: 0;
          padding: var(--bs-space-3);
        }
        .station:hover { background: var(--bs-surface-sunk); }
        .station[aria-current="true"] { background: var(--bs-surface); box-shadow: inset 3px 0 0 var(--bs-amber); }
        .station-name { display: block; font-size: 0.90625rem; }
        .station-meta { display: block; font-family: var(--bs-font-mono); font-size: 0.71875rem; color: var(--bs-text-muted); margin-top: 0.1875rem; }
        .station--new { color: var(--bs-link); font-size: 0.90625rem; border-bottom: 1px solid var(--bs-border); }
        .error { margin-bottom: var(--bs-space-4); }
        .saved { margin-top: var(--bs-space-2); text-align: center; }
        .delete, .confirm { margin-top: var(--bs-space-3); }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      <div class="columns">
        <div>
          <div class="map-slot"></div>
          <p class="note" style="margin-top: var(--bs-space-3);">
            ${
              editing
                ? `Click the map or drag the green pin to move ${escapeHTML(editing)}.
                   Coordinates can be typed beside it instead.`
                : `Click anywhere on the map to drop a pin, or drag it to correct the position.
                   Coordinates fill in beside it and can be typed instead.`
            }
          </p>
          ${status === "error" ? `<p class="note">Couldn't load the recorders: ${escapeHTML(error.message)}</p>` : ""}
        </div>

        <div class="aside">
          <form class="panel" data-submit="submit">
            <h3 style="margin-bottom: 1.125rem;">${editing ? "Edit recorder" : "New recorder"}</h3>

            <label class="label" for="station-name">Station name</label>
            <input class="field field--sunk" id="station-name" name="name" type="text" required
                   value="${escapeHTML(this.#draft.name)}" placeholder="Marymoor Park – Snag Row"
                   data-change="field" data-input="field" style="margin-bottom: var(--bs-space-4);" />

            ${
              editing
                ? `<span class="label">Recorder ID</span>
                   <div class="readout mono">${escapeHTML(editing)}<span class="tag">printed on the unit</span></div>`
                : `<label class="label" for="station-id">Recorder ID</label>
                   <input class="field field--sunk field--mono" id="station-id" name="id" type="text"
                          value="${escapeHTML(this.#draft.id)}" placeholder="SW-06"
                          data-change="field" data-input="field" style="margin-bottom: 1.125rem;" />`
            }

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
              ${this.#submitLabel()}
            </button>
            ${this.#saved ? `<p class="note saved" role="status">Changes saved.</p>` : ""}
            ${editing ? this.#deleteControls(this.#station(editing)) : ""}
          </form>

          <div>
            <div class="eyebrow" style="margin-bottom: var(--bs-space-1);">In the field</div>
            ${stations
              .map(
                (s) => `
              <button type="button" class="station" data-action="select" data-id="${escapeHTML(s.id)}"
                      ${s.id === editing ? `aria-current="true"` : ""}>
                <span class="station-name">${escapeHTML(s.name)}</span>
                <span class="station-meta">${escapeHTML(s.id)} · ${s.latitude}, ${s.longitude}</span>
              </button>`,
              )
              .join("")}
            <button type="button" class="station station--new" data-action="addNew"
                    ${editing ? "" : `aria-current="true"`}>Add new…</button>
          </div>
        </div>
      </div>
    `;

    this.$(".map-slot").replaceWith(this.#map);
    this.#map.stations = stations;
    this.#map.selected = editing;
    this.#syncMap();
  }

  #submitLabel() {
    if (this.#selected) return this.#busy && !this.#confirmingDelete ? "Saving…" : "Save changes";
    return this.#busy ? "Adding…" : "Add recorder";
  }

  #deleteControls(station) {
    if (!station) return "";
    if (!this.#confirmingDelete) {
      return `
        <button type="button" class="btn btn--quiet btn--danger btn--small btn--block delete"
                data-action="askDelete">Delete recorder</button>
      `;
    }
    return `
      <div class="confirm" role="alertdialog" aria-labelledby="confirm-text">
        <p id="confirm-text">
          Delete <strong>${escapeHTML(station.name)}</strong> (${escapeHTML(station.id)})?
          It comes off the map and out of the upload form. Cards already sent from it are kept.
        </p>
        <span class="row">
          <button type="button" class="btn btn--small btn--danger-solid" data-action="deleteRecorder"
                  ${this.#busy ? "disabled" : ""}>${this.#busy ? "Deleting…" : "Delete"}</button>
          <button type="button" class="btn btn--small btn--quiet" data-action="keepRecorder">Keep</button>
        </span>
      </div>
    `;
  }
}

customElements.define("bs-admin-recorders", AdminRecorders);
