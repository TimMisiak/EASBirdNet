import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, tables, typography } from "../shared-styles.js";
import { count, longDate } from "../format.js";
import { isMoving, isUnfinished, statusChip } from "../upload-status.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as flow from "../upload-flow.js";
import "./bs-chip.js";

/**
 * <bs-my-uploads> -- the signed-in volunteer's own cards, laid out like the
 * coordinator's list of every card (<bs-admin-uploads>), without the volunteer
 * column or the delete.
 *
 * The one thing to do here is finish a card left half-sent, because an
 * abandoned card is what costs the program a season of audio. So an unfinished
 * card's row stands out and carries "Resume upload", and the tally counts them.
 */
const POLL_MS = 10_000;

const FILTERS = [
  { id: "all", label: "All cards", match: () => true },
  { id: "unfinished", label: "Unfinished", match: isUnfinished },
  { id: "processing", label: "Processing", match: (u) => u.status === "processing" },
  { id: "review", label: "In review", match: (u) => u.status === "in_review" },
];

const COLUMNS = 9;

class MyUploads extends BaseElement {
  static styles = [typography, controls, forms, tables];

  #state = { status: "loading", uploads: [], error: null };
  #filter = "all";
  #timer = 0;
  /** The card "Resume upload" is picking up, while it loads. */
  #resuming = "";
  /** A resume that failed, shown under its row: {reference, message}. */
  #resumeError = null;

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

  async #load() {
    try {
      const { uploads } = await api.fetchMyUploads();
      this.#state = { status: "ready", uploads, error: null };
    } catch (error) {
      // A failed refresh keeps the table that's already showing.
      if (this.#state.status !== "ready") this.#state = { status: "error", uploads: [], error };
    }
    if (!this.isConnected) return;
    this.render();
    // Cards being sent or analyzed change by the minute; look again while any are.
    clearTimeout(this.#timer);
    if (this.#state.uploads.some(isMoving)) this.#timer = setTimeout(() => this.#load(), POLL_MS);
  }

  render() {
    const { status, uploads, error } = this.#state;
    const active = FILTERS.find((f) => f.id === this.#filter) ?? FILTERS[0];
    const shown = uploads.filter(active.match);
    const unfinished = uploads.filter(isUnfinished).length;

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
        td { padding-top: 0.9375rem; padding-bottom: 0.9375rem; font-size: 0.90625rem; vertical-align: middle; }
        /* The reference, the date and the counts are read as single tokens;
           only the station and the note are allowed to wrap. */
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; white-space: nowrap; }
        .num, .nowrap { white-space: nowrap; }
        .num.small { font-size: 0.84375rem; }
        .station { color: var(--bs-text-body); }
        .note-cell { font-size: 0.84375rem; color: var(--bs-text-muted); }
        /* An unfinished card is the one to act on. */
        tbody tr[data-unfinished] { background: var(--bs-notice); }
        td.actions { width: 1%; text-align: right; padding-top: 0.625rem; padding-bottom: 0.625rem; }
        .btn--tiny { padding: 0.4375rem 0.875rem; font-size: 0.8125rem; }
        tbody tr.sub { border-top: 0; }
        tr.sub td { padding-top: 0; }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
      </style>

      <div class="filters">
        ${FILTERS.map(
          (f) =>
            `<button class="filter" data-action="filter" data-filter="${f.id}"
                     aria-pressed="${f.id === active.id}">${f.label}</button>`,
        ).join("")}
        <span class="tally">
          ${count(uploads.length)} ${uploads.length === 1 ? "card" : "cards"} ·
          ${unfinished ? `${count(unfinished)} unfinished` : "none unfinished"}
        </span>
      </div>

      ${
        status === "loading"
          ? `<p class="empty">Looking up your cards…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load your cards: ${escapeHTML(error.message)}</p>`
            : uploads.length === 0
              ? `<p class="empty">No cards yet. <a href="/app/upload">Upload your first card</a>, and it shows up here.</p>`
              : shown.length === 0
                ? `<p class="empty">None of your cards match that filter.</p>`
                : `<div class="table-scroll">
                     <table>
                       <thead>
                         <tr>
                           <th scope="col">Reference</th>
                           <th scope="col">Station</th>
                           <th scope="col">Pulled</th>
                           <th scope="col" style="text-align: right;">Nights</th>
                           <th scope="col" style="text-align: right;">Files</th>
                           <th scope="col" style="text-align: right;">Detections</th>
                           <th scope="col">Status</th>
                           <th scope="col">Note</th>
                           <th scope="col" aria-label="Next step"></th>
                         </tr>
                       </thead>
                       <tbody>${shown.map((upload) => this.#row(upload)).join("")}</tbody>
                     </table>
                   </div>`
      }
    `;
  }

  /** One card's row, and under it why it couldn't be resumed, if it couldn't. */
  #row(upload) {
    const chip = statusChip(upload);
    const unfinished = isUnfinished(upload);
    const reference = escapeHTML(upload.reference);
    const row = `
      <tr ${unfinished ? "data-unfinished" : ""}>
        <td class="ref">${reference}</td>
        <td class="station">${escapeHTML(upload.stationName)}</td>
        <td class="nowrap">${escapeHTML(longDate(upload.pulledOn))}</td>
        <td class="num small">${upload.nights?.length ?? 0}</td>
        <td class="num small muted">${count(upload.filesUploaded)} / ${count(upload.fileCount)}</td>
        <td class="num small muted">${upload.analysis ? count(upload.detectionCount) : "—"}</td>
        <td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>
        <td class="note-cell">${escapeHTML(upload.notes ?? "")}</td>
        <td class="actions">${
          unfinished
            ? `<button type="button" class="btn btn--primary btn--tiny" data-action="resume" data-reference="${reference}"
                       ${this.#resuming ? "disabled" : ""}>${this.#resuming === upload.reference ? "Opening…" : "Resume upload →"}</button>`
            : ""
        }</td>
      </tr>`;
    if (this.#resumeError?.reference !== upload.reference) return row;
    return `${row}
      <tr class="sub">
        <td colspan="${COLUMNS}">
          <p class="error" role="alert">Couldn't pick this card up: ${escapeHTML(this.#resumeError.message)}</p>
        </td>
      </tr>`;
  }
}

customElements.define("bs-my-uploads", MyUploads);
