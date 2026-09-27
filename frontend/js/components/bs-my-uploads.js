import { escapeHTML } from "./base-element.js";
import { CELLS, UploadsTable, errorRow } from "./uploads-table.js";
import { count } from "../format.js";
import { isUnfinished } from "../upload-status.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as flow from "../upload-flow.js";

/**
 * <bs-my-uploads> -- the signed-in volunteer's own cards, laid out like the
 * coordinator's list of every card (<bs-admin-uploads>), without the volunteer
 * column or the delete. Both screens are uploads-table.js; this one is the
 * columns it names and the resume.
 *
 * The one thing to do here is finish a card left half-sent, because an
 * abandoned card is what costs the program a season of audio. So an unfinished
 * card's row stands out and carries "Resume upload", and the tally counts them.
 */
const FILTERS = [
  { id: "all", label: "All cards", match: () => true },
  { id: "unfinished", label: "Unfinished", match: isUnfinished },
  { id: "processing", label: "Processing", match: (u) => u.status === "processing" },
  { id: "review", label: "In review", match: (u) => u.status === "in_review" },
];

class MyUploads extends UploadsTable {
  /** The card "Resume upload" is picking up, while it loads. */
  #resuming = "";
  /** A resume that failed, shown under its row: {reference, message}. */
  #resumeError = null;

  get heading() {
    return "My uploads";
  }

  get filters() {
    return FILTERS;
  }

  get columns() {
    return [
      CELLS.reference,
      CELLS.station,
      CELLS.pulled,
      CELLS.nights,
      CELLS.files,
      CELLS.detections,
      CELLS.status,
      CELLS.note,
      { head: "Next step", headHidden: true, cell: (upload) => this.#resumeCell(upload) },
    ];
  }

  async fetch() {
    const { uploads } = await api.fetchMyUploads();
    return uploads;
  }

  get copy() {
    return {
      loading: "Looking up your cards…",
      failed: (message) => `Couldn't load your cards: ${message}`,
      none: `No cards yet. <a href="/app/upload">Upload your first card</a>, and it shows up here.`,
      noMatch: "None of your cards match that filter.",
    };
  }

  get extraStyles() {
    return `
        /* An unfinished card is the one to act on. */
        tbody tr[data-unfinished] { background: var(--bs-notice); }`;
  }

  tally() {
    const cards = this.uploads.length;
    const unfinished = this.uploads.filter(isUnfinished).length;
    return `${count(cards)} ${cards === 1 ? "card" : "cards"} ·
            ${unfinished ? `${count(unfinished)} unfinished` : "none unfinished"}`;
  }

  rowAttrs(upload) {
    return isUnfinished(upload) ? "data-unfinished" : "";
  }

  /** Under the row, why the card couldn't be picked up. */
  subRow(upload) {
    if (this.#resumeError?.reference !== upload.reference) return "";
    return errorRow(this.columnCount, `Couldn't pick this card up: ${this.#resumeError.message}`);
  }

  get actions() {
    return {
      ...super.actions,
      resume: async (el) => {
        if (this.#resuming) return;
        const reference = el.dataset.reference;
        this.#resuming = reference;
        this.#resumeError = null;
        this.render();
        try {
          navigate(await flow.resume(reference));
        } catch (error) {
          this.#resumeError = { reference, message: error.message };
        }
        this.#resuming = "";
        if (this.isConnected) this.render();
      },
    };
  }

  #resumeCell(upload) {
    if (!isUnfinished(upload)) return `<td class="actions"></td>`;
    const reference = escapeHTML(upload.reference);
    const label = this.#resuming === upload.reference ? "Opening…" : "Resume upload →";
    return `
      <td class="actions">
        <button type="button" class="btn btn--primary btn--tiny" data-action="resume" data-reference="${reference}"
                ${this.#resuming ? "disabled" : ""}>${label}</button>
      </td>`;
  }
}

customElements.define("bs-my-uploads", MyUploads);
