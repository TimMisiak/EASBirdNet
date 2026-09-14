import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, typography } from "../shared-styles.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as flow from "../upload-flow.js";
import { CancelledError, sampleCard, scanCard } from "../card-scan.js";

/**
 * <bs-upload-details> -- step 1. Four questions, none of which the volunteer
 * has to look up: the recorder comes from the station, the date defaults to
 * today, and the notes are optional.
 *
 * Choosing the card happens at the end of this step rather than the start,
 * because the folder picker is the one moment a volunteer can get stuck, and
 * it's easier to recover from with the context already filled in.
 */
class UploadDetails extends BaseElement {
  static styles = [typography, controls, forms, panels];

  #stations = [];
  #busy = false;
  #error = null;

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  get actions() {
    return {
      station: (el) => {
        flow.setDetails({ stationId: el.value });
        this.render();
      },
      pulled: (el) => flow.setDetails({ pulledOn: el.value }),
      notes: (el) => flow.setDetails({ notes: el.value }),
      choose: () => this.#choose(() => scanCard()),
      sample: () => this.#choose(async () => sampleCard()),
    };
  }

  async #load() {
    try {
      const { stations } = await api.fetchStations();
      this.#stations = stations;
      if (!flow.get().stationId && stations.length) {
        flow.setDetails({ stationId: stations[0].id });
      }
    } catch (error) {
      this.#error = error;
    }
    if (this.isConnected) this.render();
  }

  async #choose(read) {
    if (this.#busy) return;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      const card = await read();
      if (!card.fileCount) {
        throw new Error("No audio files on that card — is it the right folder?");
      }
      await flow.registerCard(card);
      navigate("/app/upload/check");
    } catch (error) {
      this.#busy = false;
      // Closing the picker isn't a failure; it's a change of mind.
      this.#error = error instanceof CancelledError ? null : error;
      this.render();
    }
  }

  render() {
    const { stationId, pulledOn, notes } = flow.get();
    const station = this.#stations.find((s) => s.id === stationId);

    this.shadowRoot.innerHTML = `
      <style>
        h1 { margin-bottom: 0.375rem; }
        .intro { margin-bottom: 2.125rem; max-width: 70ch; }
        .columns {
          display: grid;
          grid-template-columns: minmax(0, 1fr) minmax(0, 0.85fr);
          gap: 2.75rem;
          align-items: start;
        }
        .form { display: flex; flex-direction: column; gap: 1.375rem; }
        .aside { display: flex; flex-direction: column; gap: 1.125rem; }
        .aside ul {
          margin: 0;
          padding-left: 1.125rem;
          display: flex;
          flex-direction: column;
          gap: 0.5625rem;
          font-size: 0.875rem;
          line-height: 1.55;
          color: var(--bs-text-soft);
        }
        .step-footer .row { align-items: center; gap: var(--bs-space-4); }
        .sample { font-size: 0.8125rem; }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      <h1>Upload an SD card</h1>
      <p class="lede intro">
        Put the SD card in your reader and leave it there. Files upload straight from the
        card — nothing is copied to your computer, and nothing on the card is changed.
      </p>

      <div class="columns">
        <div class="form">
          <div>
            <label class="label" for="station">Where was this card collected?</label>
            <select class="field" id="station" data-change="station" ${this.#busy ? "disabled" : ""}>
              ${this.#stations
                .map(
                  (s) =>
                    `<option value="${escapeHTML(s.id)}" ${s.id === stationId ? "selected" : ""}>${escapeHTML(s.name)}</option>`,
                )
                .join("")}
            </select>
          </div>

          <div>
            <span class="label">Recorder</span>
            <div class="readout">
              <span class="mono">${escapeHTML(station?.id ?? "—")}</span>
              <span class="tag">filled in from the station</span>
            </div>
          </div>

          <div>
            <label class="label" for="pulled">Date you pulled the card</label>
            <input class="field" id="pulled" type="date" value="${escapeHTML(pulledOn)}"
                   max="${escapeHTML(new Date().toISOString().slice(0, 10))}" data-change="pulled" />
          </div>

          <div>
            <label class="label" for="notes">Notes <span class="optional">(optional)</span></label>
            <textarea class="field" id="notes" rows="4" data-change="notes"
                      placeholder="Anything we should know — batteries were dead, recorder was knocked over, heavy rain on the 3rd…">${escapeHTML(notes)}</textarea>
          </div>

          ${this.#error ? `<p class="error">${escapeHTML(this.#error.message)}</p>` : ""}
        </div>

        <div class="aside">
          <div class="panel panel--parchment">
            <h3>Before you start</h3>
            <ul>
              <li>Card in the reader, reader plugged into this computer</li>
              <li>Laptop plugged into power</li>
              <li>A full card (about 14 nights, ~128 GB) takes roughly 2–3 hours on home Wi-Fi — wired is faster</li>
              <li>Keep this tab open; you can use other apps meanwhile</li>
            </ul>
          </div>
          <div class="panel">
            <h3>If it gets interrupted</h3>
            <p>
              No problem. Come back to this page, choose the card again, and we upload only
              what's missing. Keep the card until you get the confirmation email.
            </p>
          </div>
        </div>
      </div>

      <div class="step-footer">
        <span class="note">
          Next you'll point us at the card.
          <a class="sample" href="#sample" data-action="sample">No card handy? Use a sample card</a>
        </span>
        <button class="btn btn--primary" data-action="choose" ${this.#busy || !station ? "disabled" : ""}>
          ${this.#busy ? "Reading the card…" : "Choose the SD card →"}
        </button>
      </div>
    `;
  }
}

customElements.define("bs-upload-details", UploadDetails);
