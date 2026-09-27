import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, filters, forms, panels, tables, typography } from "../shared-styles.js";
import { count, longDate } from "../format.js";
import { isMoving, statusChip } from "../upload-status.js";
import "./bs-chip.js";

/**
 * The list of cards, which two screens show: the volunteer's own
 * (<bs-my-uploads>) and every card in the program (<bs-admin-uploads>).
 *
 * They are one table. Same filter pills and tally, same columns of reference,
 * station, counts, status and note, same sub-row under a row with more to say,
 * same ten-second poll while anything is moving. They differ in which columns
 * they name, what the last one does, and the wording around an empty list --
 * so that is what a subclass supplies, and a fix to the rest fixes both.
 *
 * Not a custom element: the base class the two are built from, like
 * base-element.js.
 */

/** Cards being sent or analyzed change by the minute; look again while any are. */
const POLL_MS = 10_000;

/**
 * The columns both lists draw from. A column is
 * `{head, right?, headHidden?, cell(upload)}` -- a screen's own trailing
 * action column is one of these too, and lives with that screen.
 */
export const CELLS = {
  reference: {
    head: "Reference",
    cell: (u) => `<td class="ref">${escapeHTML(u.reference)}</td>`,
  },
  /** The same column on the coordinator's list, where it opens the card. */
  referenceLink: {
    head: "Reference",
    cell: (u) =>
      `<td class="ref"><a href="/admin/uploads/${encodeURIComponent(u.reference)}">${escapeHTML(u.reference)}</a></td>`,
  },
  volunteer: {
    head: "Volunteer",
    cell: (u) => `<td class="nowrap">${escapeHTML(u.volunteerName)}</td>`,
  },
  station: {
    head: "Station",
    cell: (u) => `<td class="station">${escapeHTML(u.stationName)}</td>`,
  },
  pulled: {
    head: "Pulled",
    cell: (u) => `<td class="nowrap">${escapeHTML(longDate(u.pulledOn))}</td>`,
  },
  nights: {
    head: "Nights",
    right: true,
    cell: (u) => `<td class="num">${u.nights?.length ?? 0}</td>`,
  },
  files: {
    head: "Files",
    right: true,
    cell: (u) => `<td class="num muted">${count(u.filesUploaded)} / ${count(u.fileCount)}</td>`,
  },
  detections: {
    head: "Detections",
    right: true,
    cell: (u) => `<td class="num muted">${u.analysis ? count(u.detectionCount) : "—"}</td>`,
  },
  status: {
    head: "Status",
    cell: (u) => {
      const chip = statusChip(u);
      return `<td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>`;
    },
  },
  note: {
    head: "Note",
    cell: (u) => `<td class="note-cell">${escapeHTML(u.notes ?? "")}</td>`,
  },
};

export class UploadsTable extends BaseElement {
  static styles = [typography, controls, forms, panels, tables, filters];

  #state = { status: "loading", uploads: [], error: null };
  #filter = "";
  #timer = 0;

  // What a subclass supplies. `filters` and `columns` are required, with
  // `fetch()`; everything below them is a screen's own part, or nothing.

  /** The filter pills, `[{id, label, match}]`. The first one is the default. */
  get filters() {
    return [];
  }

  /** The columns, in order: entries of CELLS, plus the screen's own. */
  get columns() {
    return [];
  }

  /** The page's heading. The shell's greeting isn't one, so this is the <h1>. */
  get heading() {
    return "";
  }

  /** Load the list. Throwing keeps the table that is already showing. */
  async fetch() {
    throw new Error("uploads-table: a subclass supplies fetch()");
  }

  /** Loading, failed and empty wording. `failed` is handed escaped text. */
  get copy() {
    return {
      loading: "Loading cards…",
      failed: (message) => `Couldn't load the cards: ${message}`,
      /** No cards at all -- trusted HTML, so it can carry a link. */
      none: "",
      noMatch: "No cards match that filter.",
    };
  }

  /** CSS for the screen's own columns and actions. */
  get extraStyles() {
    return "";
  }

  /** Trusted HTML above the filter row. */
  get header() {
    return "";
  }

  /** The right-hand end of the filter row: what the list holds. */
  tally() {
    return "";
  }

  /** Attributes for one card's `<tr>`. */
  rowAttrs() {
    return "";
  }

  /** Trusted HTML for the row under a card's, when it has more to say. */
  subRow() {
    return "";
  }

  /**
   * Whether a refresh should leave the screen alone: something is open that a
   * re-render would close, or take the focus off.
   */
  get holdRender() {
    return false;
  }

  // The parts they share.

  connectedCallback() {
    super.connectedCallback();
    this.load();
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
    };
  }

  /** Fetch the list, show it, and keep looking while anything is moving. */
  async load() {
    try {
      this.#state = { status: "ready", uploads: await this.fetch(), error: null };
    } catch (error) {
      // A failed refresh keeps the table that's already showing.
      if (this.#state.status !== "ready") this.#state = { status: "error", uploads: [], error };
    }
    if (!this.isConnected) return;
    if (!this.holdRender) this.render();
    clearTimeout(this.#timer);
    if (this.#state.uploads.some(isMoving)) this.#timer = setTimeout(() => this.load(), POLL_MS);
  }

  /** The cards the table has, whatever the filter is. */
  get uploads() {
    return this.#state.uploads;
  }

  /** How many columns wide the table is, for a sub-row's colspan. */
  get columnCount() {
    return this.columns.length;
  }

  render() {
    const { status, uploads, error } = this.#state;
    const pills = this.filters;
    const active = pills.find((f) => f.id === this.#filter) ?? pills[0];
    const shown = uploads.filter(active.match);
    const copy = this.copy;

    this.shadowRoot.innerHTML = `
      <style>
        h1 { margin-bottom: var(--bs-space-5); }
        .filters { margin-bottom: var(--bs-space-5); }
        th { padding-top: 0; }
        td { padding-top: 0.9375rem; padding-bottom: 0.9375rem; font-size: 0.90625rem; vertical-align: middle; }
        /* The reference, the date and the counts are read as single tokens;
           only the station and the note are allowed to wrap. */
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; white-space: nowrap; }
        .num, .nowrap { white-space: nowrap; }
        .num { font-size: 0.84375rem; }
        .station { color: var(--bs-text-body); }
        .note-cell { font-size: 0.84375rem; color: var(--bs-text-muted); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        td.actions { width: 1%; text-align: right; padding-top: 0.625rem; padding-bottom: 0.625rem; }
        /* A sub-row belongs to the row above it: no rule between them. */
        tbody tr.sub { border-top: 0; }
        tbody tr.sub:hover { background: none; }
        tr.sub td { padding-top: 0; }
        ${this.extraStyles}
      </style>

      <h1>${this.heading}</h1>
      ${this.header}

      <div class="filters">
        ${pills
          .map(
            (f) =>
              `<button class="filter" data-action="filter" data-filter="${f.id}"
                       aria-pressed="${f.id === active.id}">${f.label}</button>`,
          )
          .join("")}
        <span class="tally">${this.tally()}</span>
      </div>

      ${
        status === "loading"
          ? `<p class="empty">${copy.loading}</p>`
          : status === "error"
            ? `<p class="empty">${copy.failed(escapeHTML(error.message))}</p>`
            : uploads.length === 0 && copy.none
              ? `<p class="empty">${copy.none}</p>`
              : shown.length === 0
                ? `<p class="empty">${copy.noMatch}</p>`
                : this.#table(shown)
      }
    `;
  }

  #table(shown) {
    const columns = this.columns;
    const rows = shown.map(
      (upload) =>
        `<tr ${this.rowAttrs(upload)}>${columns.map((c) => c.cell(upload)).join("")}</tr>` + this.subRow(upload),
    );
    return `
      <div class="table-scroll">
        <table>
          <thead><tr>${columns.map(head).join("")}</tr></thead>
          <tbody>${rows.join("")}</tbody>
        </table>
      </div>`;
  }
}

/**
 * A column's header. `headHidden` is for a column of buttons: the header is
 * still there for a screen reader, which otherwise reaches an unnamed column.
 */
function head(column) {
  const label = column.headHidden ? `<span class="visually-hidden">${column.head}</span>` : column.head;
  return `<th scope="col"${column.right ? ` style="text-align: right;"` : ""}>${label}</th>`;
}

/** The error shown under the row it belongs to. */
export function errorRow(columnCount, message) {
  return `
    <tr class="sub">
      <td colspan="${columnCount}"><p class="error" role="alert">${escapeHTML(message)}</p></td>
    </tr>`;
}
