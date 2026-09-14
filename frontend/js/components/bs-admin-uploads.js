import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, tables, typography } from "../shared-styles.js";
import { count } from "../format.js";
import { isUnfinished, statusChip } from "../upload-status.js";
import * as api from "../api.js";
import "./bs-chip.js";

/**
 * <bs-admin-uploads> -- every card in the program. A coordinator opens this to
 * find the two that are stuck, so the filters lead with that and the count of
 * cards needing attention is stated rather than left to be counted.
 */
const FILTERS = [
  { id: "all", label: "All cards", match: () => true },
  { id: "attention", label: "Needs attention", match: (u) => u.status === "needs_attention" },
  { id: "unfinished", label: "Unfinished", match: isUnfinished },
  { id: "processing", label: "Processing", match: (u) => u.status === "processing" },
];

class AdminUploads extends BaseElement {
  static styles = [typography, controls, tables];

  #state = { status: "loading", uploads: [], error: null };
  #filter = "all";

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  get actions() {
    return {
      filter: (el) => {
        this.#filter = el.dataset.filter;
        this.render();
      },
    };
  }

  async #load() {
    try {
      const { uploads } = await api.fetchAllUploads();
      this.#state = { status: "ready", uploads, error: null };
    } catch (error) {
      this.#state = { status: "error", uploads: [], error };
    }
    if (this.isConnected) this.render();
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
        .note-cell { font-size: 0.84375rem; color: var(--bs-text-muted); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
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
                         <th scope="col">Status</th>
                         <th scope="col">Note</th>
                       </tr>
                     </thead>
                     <tbody>${shown.map(row).join("")}</tbody>
                   </table>
                 </div>`
      }
    `;
  }
}

function row(upload) {
  const chip = statusChip(upload);
  return `
    <tr>
      <td class="ref">${escapeHTML(upload.reference)}</td>
      <td class="who">${escapeHTML(upload.volunteerName)}</td>
      <td style="color: var(--bs-text-body);">${escapeHTML(upload.stationName)}</td>
      <td class="num" style="font-size: 0.84375rem;">${upload.nights?.length ?? 0}</td>
      <td class="num muted" style="font-size: 0.84375rem;">${count(upload.filesUploaded)} / ${count(upload.fileCount)}</td>
      <td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>
      <td class="note-cell">${escapeHTML(upload.notes ?? "")}</td>
    </tr>
  `;
}

customElements.define("bs-admin-uploads", AdminUploads);
