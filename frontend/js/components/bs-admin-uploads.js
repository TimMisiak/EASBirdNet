import { escapeHTML } from "./base-element.js";
import { CELLS, UploadsTable, errorRow } from "./uploads-table.js";
import { count } from "../format.js";
import { isUnfinished } from "../upload-status.js";
import * as api from "../api.js";
import "./bs-queue-note.js";

/**
 * <bs-admin-uploads> -- every card in the program, on the same table the
 * volunteer's own list uses (uploads-table.js, <bs-my-uploads>), with the
 * volunteer column and the delete. A coordinator opens this to find the two
 * that are stuck, so the filters lead with that and the count of cards needing
 * attention is stated rather than left to be counted. A row opens the card's
 * own page, with every file and what BirdNET heard in it.
 *
 * The bin at the end of a row deletes the card for good, after a confirmation
 * that says what goes with it: its audio and every detection in it.
 */
const FILTERS = [
  { id: "all", label: "All cards", match: () => true },
  { id: "attention", label: "Needs attention", match: (u) => u.status === "needs_attention" },
  { id: "unfinished", label: "Unfinished", match: isUnfinished },
  { id: "processing", label: "Processing", match: (u) => u.status === "processing" },
  { id: "review", label: "In review", match: (u) => u.status === "in_review" },
];

class AdminUploads extends UploadsTable {
  /** Where the analysis queue stands, shown above the cards it explains. */
  #queue = null;
  /** The reference of the row asking "delete this card?". */
  #confirming = null;
  /** A failed delete, shown under that row: {reference, message}. */
  #rowError = null;
  #busy = false;

  constructor() {
    super();
    this.shadowRoot.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && this.#confirming) this.actions.cancel();
    });
  }

  get heading() {
    return "All uploads";
  }

  get filters() {
    return FILTERS;
  }

  get columns() {
    return [
      CELLS.referenceLink,
      CELLS.volunteer,
      CELLS.station,
      CELLS.nights,
      CELLS.files,
      CELLS.detections,
      CELLS.status,
      CELLS.note,
      { head: "Actions", headHidden: true, cell: binCell },
    ];
  }

  async fetch() {
    const { uploads, queue } = await api.fetchAllUploads();
    this.#queue = queue;
    return uploads;
  }

  get header() {
    return `<bs-queue-note></bs-queue-note>`;
  }

  render() {
    super.render();
    this.$("bs-queue-note").queue = this.#queue;
  }

  get extraStyles() {
    return `
        /* The reference links to the card, and its link covers the row. */
        tbody tr { position: relative; }
        tbody tr:hover { background: var(--bs-surface-sunk); }
        .ref a { color: var(--bs-text); text-decoration: none; }
        .ref a::after { content: ""; position: absolute; inset: 0; }
        tbody tr:hover .ref a { text-decoration: underline; }
        /* Above the reference's link, which covers the rest of the row. */
        .bin { position: relative; z-index: 1; padding: 0.4375rem; border-color: transparent; }
        .bin svg { display: block; }`;
  }

  tally() {
    const needing = this.uploads.filter((u) => u.status === "needs_attention").length;
    return `${count(this.uploads.length)} cards ·
            ${needing ? `${count(needing)} need attention` : "none need attention"}`;
  }

  /** Under the row, the confirmation it is waiting on or the delete that failed. */
  subRow(upload) {
    if (this.#confirming === upload.reference) return this.#confirmRow(upload);
    if (this.#rowError?.reference === upload.reference) return errorRow(this.columnCount, this.#rowError.message);
    return "";
  }

  /** A refresh would take the focus off an open confirmation; it shows when that closes. */
  get holdRender() {
    return this.#confirming !== null;
  }

  get actions() {
    return {
      ...super.actions,

      askDelete: (el) => {
        this.#confirming = el.dataset.reference;
        this.#rowError = null;
        this.render();
        // Focus the safe choice, so an Enter doesn't delete anything.
        this.$('[data-action="cancel"]')?.focus();
      },
      cancel: () => {
        this.#confirming = null;
        this.#rowError = null;
        this.render();
      },
      confirmDelete: async () => {
        const reference = this.#confirming;
        if (!reference || this.#busy) return;
        this.#busy = true;
        this.render();
        try {
          await api.deleteUpload(reference);
        } catch (error) {
          this.#rowError = { reference, message: error.message };
        } finally {
          this.#confirming = null;
          this.#busy = false;
        }
        await this.load();
      },
    };
  }

  #confirmRow(upload) {
    // Retention may already have taken the recordings; the detections and
    // their clips are only ever lost here, which is the part to spell out.
    const files = upload.audioDeletedAt ? 0 : (upload.filesUploaded ?? 0);
    const lost = [];
    if (files) lost.push(`${count(files)} audio ${files === 1 ? "file" : "files"}`);
    if (upload.analysis) {
      lost.push(`${count(upload.detectionCount)} ${upload.detectionCount === 1 ? "detection" : "detections"}`, "their clips");
    }
    const goes = lost.length ? ` Its ${LIST.format(lost)} are deleted with it.` : "";
    const sending = isUnfinished(upload) ? " The volunteer's upload stops, and they would have to send the card again." : "";
    return `
      <tr class="sub">
        <td colspan="${this.columnCount}">
          <div class="confirm" role="alertdialog" aria-labelledby="confirm-text">
            <p id="confirm-text">
              Delete <strong>${escapeHTML(upload.reference)}</strong> for good?${goes}${sending}
            </p>
            <span class="row">
              <button type="button" class="btn btn--tiny btn--danger-solid" data-action="confirmDelete"
                      ${this.#busy ? "disabled" : ""}>${this.#busy ? "Deleting…" : "Delete"}</button>
              <button type="button" class="btn btn--quiet btn--tiny" data-action="cancel">Keep</button>
            </span>
          </div>
        </td>
      </tr>
    `;
  }
}

/** "a, b, and c" -- what a delete takes with it can be two or three things. */
const LIST = new Intl.ListFormat("en-US", { style: "long", type: "conjunction" });

const BIN = `
  <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor"
       stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">
    <path d="M2.5 4h11M6.25 4V2.5h3.5V4M3.75 4l.75 9.5h7l.75-9.5M6.5 6.75v4.5M9.5 6.75v4.5" />
  </svg>`;

function binCell(upload) {
  const reference = escapeHTML(upload.reference);
  return `
    <td class="actions">
      <button type="button" class="btn btn--quiet btn--danger bin" data-action="askDelete"
              data-reference="${reference}" aria-label="Delete card ${reference}" title="Delete card">${BIN}</button>
    </td>`;
}

customElements.define("bs-admin-uploads", AdminUploads);
