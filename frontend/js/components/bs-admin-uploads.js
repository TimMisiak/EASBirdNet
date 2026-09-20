import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { count } from "../format.js";
import { isMoving, isUnfinished, statusChip } from "../upload-status.js";
import * as api from "../api.js";
import "./bs-chip.js";

/**
 * <bs-admin-uploads> -- every card in the program. A coordinator opens this to
 * find the two that are stuck, so the filters lead with that and the count of
 * cards needing attention is stated rather than left to be counted. A row
 * opens the card's own page, with every file and what BirdNET heard in it.
 *
 * The bin at the end of a row deletes the card for good, after a confirmation
 * that says what goes with it: its audio and every detection in it.
 */
const POLL_MS = 10_000;

const FILTERS = [
  { id: "all", label: "All cards", match: () => true },
  { id: "attention", label: "Needs attention", match: (u) => u.status === "needs_attention" },
  { id: "unfinished", label: "Unfinished", match: isUnfinished },
  { id: "processing", label: "Processing", match: (u) => u.status === "processing" },
  { id: "review", label: "In review", match: (u) => u.status === "in_review" },
];

class AdminUploads extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];

  #state = { status: "loading", uploads: [], error: null };
  #filter = "all";
  #timer = 0;

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

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  disconnectedCallback() {
    clearTimeout(this.#timer);
  }

  get actions() {
    return {
      filter: (el) => {
        this.#filter = el.dataset.filter;
        this.render();
      },

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
        await this.#load();
      },
    };
  }

  async #load() {
    try {
      const { uploads } = await api.fetchAllUploads();
      this.#state = { status: "ready", uploads, error: null };
    } catch (error) {
      // A failed refresh keeps the table that's already showing.
      if (this.#state.status !== "ready") this.#state = { status: "error", uploads: [], error };
    }
    if (!this.isConnected) return;
    // A refresh would take the focus off an open confirmation; it shows when that closes.
    if (!this.#confirming) this.render();
    // Cards being sent or analyzed change by the minute; look again while any are.
    clearTimeout(this.#timer);
    if (this.#state.uploads.some(isMoving)) this.#timer = setTimeout(() => this.#load(), POLL_MS);
  }

  render() {
    const { status, uploads, error } = this.#state;
    const active = FILTERS.find((f) => f.id === this.#filter) ?? FILTERS[0];
    const shown = uploads.filter(active.match);
    const needing = uploads.filter((u) => u.status === "needs_attention").length;

    this.shadowRoot.innerHTML = `
      <style>
        .filters { display: flex; align-items: center; gap: var(--bs-space-3); margin-bottom: var(--bs-space-5); flex-wrap: wrap; }
        .filter {
          font-size: 0.8125rem;
          padding: 0.375rem 0.875rem;
          border-radius: var(--bs-radius-pill);
          border: 1px solid var(--bs-border-strong);
          background: transparent;
          color: var(--bs-text-body);
        }
        .filter[aria-pressed="true"] { background: var(--bs-text); border-color: var(--bs-text); color: var(--bs-on-forest); }
        .tally { margin-left: auto; font-size: 0.8125rem; color: var(--bs-text-muted); }
        th { padding-top: 0; }
        td { padding-top: 0.9375rem; padding-bottom: 0.9375rem; font-size: 0.90625rem; }
        /* The reference and the counts are read as single tokens; only the
           station and the note are allowed to wrap. */
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; white-space: nowrap; }
        .num, .who { white-space: nowrap; }
        /* The reference links to the card, and its link covers the row. */
        tbody tr { position: relative; }
        tbody tr:hover { background: var(--bs-surface-sunk); }
        .ref a { color: var(--bs-text); text-decoration: none; }
        .ref a::after { content: ""; position: absolute; inset: 0; }
        tbody tr:hover .ref a { text-decoration: underline; }
        .note-cell { font-size: 0.84375rem; color: var(--bs-text-muted); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }

        .visually-hidden {
          position: absolute;
          width: 1px;
          height: 1px;
          overflow: hidden;
          clip-path: inset(50%);
          white-space: nowrap;
        }
        td.actions { width: 1%; text-align: right; padding-top: 0.625rem; padding-bottom: 0.625rem; }
        /* Above the reference's link, which covers the rest of the row. */
        .bin { position: relative; z-index: 1; padding: 0.4375rem; border-color: transparent; }
        .bin svg { display: block; }
        tbody tr.sub { border-top: 0; }
        tbody tr.sub:hover { background: none; }
        tr.sub td { padding-top: 0; }
        .btn--tiny { padding: 0.375rem 0.75rem; font-size: 0.8125rem; }
      </style>

      <div class="filters">
        ${FILTERS.map(
          (f) =>
            `<button class="filter" data-action="filter" data-filter="${f.id}"
                     aria-pressed="${f.id === active.id}">${f.label}</button>`,
        ).join("")}
        <span class="tally">
          ${count(uploads.length)} cards · ${needing ? `${count(needing)} need attention` : "none need attention"}
        </span>
      </div>

      ${
        status === "loading"
          ? `<p class="empty">Loading cards…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load the cards: ${escapeHTML(error.message)}</p>`
            : shown.length === 0
              ? `<p class="empty">No cards match that filter.</p>`
              : `<div class="table-scroll">
                   <table>
                     <thead>
                       <tr>
                         <th scope="col">Reference</th>
                         <th scope="col">Volunteer</th>
                         <th scope="col">Station</th>
                         <th scope="col" style="text-align: right;">Nights</th>
                         <th scope="col" style="text-align: right;">Files</th>
                         <th scope="col" style="text-align: right;">Detections</th>
                         <th scope="col">Status</th>
                         <th scope="col">Note</th>
                         <th scope="col"><span class="visually-hidden">Actions</span></th>
                       </tr>
                     </thead>
                     <tbody>${shown.map((upload) => this.#row(upload)).join("")}</tbody>
                   </table>
                 </div>`
      }
    `;
  }

  /** One card's row, plus the confirmation or error row under it if any. */
  #row(upload) {
    const main = row(upload);
    if (this.#confirming === upload.reference) return main + this.#confirmRow(upload);
    if (this.#rowError?.reference === upload.reference) {
      return `${main}
        <tr class="sub">
          <td colspan="${COLUMNS}"><p class="error" role="alert">${escapeHTML(this.#rowError.message)}</p></td>
        </tr>`;
    }
    return main;
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
        <td colspan="${COLUMNS}">
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

const COLUMNS = 9;

/** "a, b, and c" -- what a delete takes with it can be two or three things. */
const LIST = new Intl.ListFormat("en-US", { style: "long", type: "conjunction" });

const BIN = `
  <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor"
       stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">
    <path d="M2.5 4h11M6.25 4V2.5h3.5V4M3.75 4l.75 9.5h7l.75-9.5M6.5 6.75v4.5M9.5 6.75v4.5" />
  </svg>`;

function row(upload) {
  const chip = statusChip(upload);
  const reference = escapeHTML(upload.reference);
  return `
    <tr>
      <td class="ref"><a href="/admin/uploads/${encodeURIComponent(upload.reference)}">${reference}</a></td>
      <td class="who">${escapeHTML(upload.volunteerName)}</td>
      <td style="color: var(--bs-text-body);">${escapeHTML(upload.stationName)}</td>
      <td class="num" style="font-size: 0.84375rem;">${upload.nights?.length ?? 0}</td>
      <td class="num muted" style="font-size: 0.84375rem;">${count(upload.filesUploaded)} / ${count(upload.fileCount)}</td>
      <td class="num muted" style="font-size: 0.84375rem;">${upload.analysis ? count(upload.detectionCount) : "—"}</td>
      <td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>
      <td class="note-cell">${escapeHTML(upload.notes ?? "")}</td>
      <td class="actions">
        <button type="button" class="btn btn--quiet btn--danger bin" data-action="askDelete"
                data-reference="${reference}" aria-label="Delete card ${reference}" title="Delete card">${BIN}</button>
      </td>
    </tr>
  `;
}

customElements.define("bs-admin-uploads", AdminUploads);
