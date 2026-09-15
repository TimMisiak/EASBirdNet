import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, typography } from "../shared-styles.js";
import { byteSize, count, longDate } from "../format.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as flow from "../upload-flow.js";
import { CancelledError, scanCard } from "../card-scan.js";

/**
 * <bs-upload-details> -- step 1. Four questions, none of which the volunteer
 * has to look up: the recorder comes from the station, the date defaults to
 * today, and the notes are optional.
 *
 * Choosing the card happens at the end of this step rather than the start,
 * because the folder picker is the one moment a volunteer can get stuck, and
 * it's easier to recover from with the context already filled in.
 *
 * Finishing a card that is already on the server starts here too. Its station
 * and pull date are what make it that card, so they are shown rather than
 * asked, and the step is choosing the card again. Before the card is registered
 * again, the files the server already has are looked for in the chosen folder:
 * registering a different folder would take them off the card's list.
 */
class UploadDetails extends BaseElement {
  static styles = [typography, controls, forms, panels];

  #stations = [];
  #busy = false;
  #error = null;
  /** {card, missing}: a folder without some of the files already uploaded, until the volunteer decides. */
  #mismatch = null;

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
      choose: () => this.#choose(),
      anyway: () => {
        const { card } = this.#mismatch;
        this.#work(() => this.#register(card));
      },
      different: () => {
        flow.reset();
        if (this.#stations.length) flow.setDetails({ stationId: this.#stations[0].id });
        this.#mismatch = null;
        this.#error = null;
        this.render();
      },
    };
  }

  async #load() {
    try {
      // A reload part-way through finishing a card picks the card back up.
      const [{ stations }] = await Promise.all([api.fetchStations(), flow.current()]);
      this.#stations = stations;
      if (!flow.get().stationId && stations.length) {
        flow.setDetails({ stationId: stations[0].id });
      }
    } catch (error) {
      this.#error = error;
    }
    if (this.isConnected) this.render();
  }

  #choose() {
    return this.#work(async () => {
      const card = await scanCard();
      if (!card.fileCount) {
        throw new Error("No audio files on that card — is it the right folder?");
      }
      if (flow.resumingCard()) {
        const missing = await flow.storedFilesMissingFrom(card);
        if (missing.length) {
          this.#mismatch = { card, missing };
          return;
        }
      }
      await this.#register(card);
    });
  }

  async #register(card) {
    await flow.registerCard(card);
    navigate(flow.get().status === "done" ? "/app/upload/done" : "/app/upload/check");
  }

  /** Run a step with the button held down, and show what went wrong, if anything. */
  async #work(step) {
    if (this.#busy) return;
    this.#busy = true;
    this.#error = null;
    this.#mismatch = null;
    this.render();
    try {
      await step();
    } catch (error) {
      // Closing the picker isn't a failure; it's a change of mind.
      this.#error = error instanceof CancelledError ? null : error;
    }
    this.#busy = false;
    if (this.isConnected) this.render();
  }

  render() {
    const resuming = flow.resumingCard();

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
        .mismatch p { font-size: 0.875rem; line-height: 1.55; margin-bottom: var(--bs-space-3); }
        .mismatch .mono { overflow-wrap: anywhere; }
        .step-footer .row { align-items: center; gap: var(--bs-space-4); }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      ${resuming ? this.#resume(resuming) : this.#fresh()}
    `;
  }

  #fresh() {
    const { stationId, pulledOn, notes } = flow.get();
    const station = this.#stations.find((s) => s.id === stationId);

    return `
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
                   max="${escapeHTML(flow.today())}" data-change="pulled" />
          </div>

          ${notesField(notes)}
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
              No problem. Come back to your cards, pick this one and choose the card again,
              and we upload only what's missing. Keep the card until you get the confirmation email.
            </p>
          </div>
        </div>
      </div>

      <div class="step-footer">
        <span class="note">Next you'll point us at the card.</span>
        ${this.#chooseButton(!station)}
      </div>
    `;
  }

  #resume(upload) {
    return `
      <h1>Finish uploading a card</h1>
      <p class="lede intro">
        Put the same SD card back in your reader and choose it again. We'll check which
        files already made it and upload only the ones that are still missing.
      </p>

      <div class="columns">
        <div class="form">
          <div>
            <span class="label">Where this card was collected</span>
            <div class="readout">
              ${escapeHTML(upload.stationName)} · <span class="mono">${escapeHTML(upload.stationId)}</span>
            </div>
          </div>

          <div>
            <span class="label">Date you pulled the card</span>
            <div class="readout">${escapeHTML(longDate(upload.pulledOn))}</div>
          </div>

          <div>
            <span class="label">Uploaded so far</span>
            <div class="readout">
              ${count(upload.filesUploaded)} of ${count(upload.fileCount)} files ·
              ${byteSize(upload.bytesUploaded)} of ${byteSize(upload.totalBytes)}
            </div>
          </div>

          ${notesField(flow.get().notes)}
          ${this.#mismatch ? this.#mismatchNotice() : ""}
          ${this.#error ? `<p class="error">${escapeHTML(this.#error.message)}</p>` : ""}
        </div>

        <div class="aside">
          <div class="panel panel--parchment">
            <h3>Choosing the card again</h3>
            <ul>
              <li>Choose the same folder as last time: the card itself, not a folder on it</li>
              <li>Files that already made it are recognised by their name and size, and skipped</li>
              <li>In the browser you started in, a file that stopped part-way carries on from where it got to</li>
            </ul>
          </div>
          <div class="panel">
            <h3>Not this card?</h3>
            <p>
              This one stays on your list to finish later.
              <a href="/app/upload" data-action="different">Upload a different card instead</a>.
            </p>
          </div>
        </div>
      </div>

      <div class="step-footer">
        <span class="note">Reference <span class="mono">${escapeHTML(upload.reference)}</span></span>
        ${this.#chooseButton(false)}
      </div>
    `;
  }

  #mismatchNotice() {
    const { card, missing } = this.#mismatch;
    const one = missing.length === 1;
    return `
      <div class="panel panel--notice mismatch" role="alert">
        <h3>That doesn't look like the same card</h3>
        <p>
          ${one ? "One file" : `${count(missing.length)} files`} we already have from this card
          ${one ? "isn't" : "aren't"} in <span class="mono">${escapeHTML(card.label)}</span> at the
          same place and size, such as <span class="mono">${escapeHTML(missing[0].path)}</span>.
          If it is the same card, choose it again, picking the card itself rather than a folder on it.
        </p>
        <p>
          Using this folder anyway makes it the card's file list: ${one ? "that file stops" : "those files stop"}
          counting, and everything in the folder we don't have is uploaded.
        </p>
        <div class="row">
          <button class="btn btn--primary btn--small" data-action="choose">Choose again</button>
          <button class="btn btn--quiet btn--small" data-action="anyway">Use this folder anyway</button>
        </div>
      </div>
    `;
  }

  #chooseButton(disabled) {
    return `
      <button class="btn btn--primary" data-action="choose" ${this.#busy || disabled ? "disabled" : ""}>
        ${this.#busy ? "Reading the card…" : "Choose the SD card →"}
      </button>
    `;
  }
}

function notesField(notes) {
  return `
    <div>
      <label class="label" for="notes">Notes <span class="optional">(optional)</span></label>
      <textarea class="field" id="notes" rows="4" data-change="notes"
                placeholder="Anything we should know — batteries were dead, recorder was knocked over, heavy rain on the 3rd…">${escapeHTML(notes)}</textarea>
    </div>
  `;
}

customElements.define("bs-upload-details", UploadDetails);
