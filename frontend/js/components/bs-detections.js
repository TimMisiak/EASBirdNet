import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, tables, typography } from "../shared-styles.js";
import { count, dateAtTime } from "../format.js";
import { reviewChip } from "../upload-status.js";
import { path, query, replaceQuery } from "../router.js";
import * as api from "../api.js";
import * as session from "../session.js";
import "./bs-chip.js";

/**
 * <bs-detections> -- everything BirdNET has heard, on every card: sorted
 * by when, species or confidence, and filtered by review, species, confidence
 * and the days it was heard. A row opens the detection's own page, to hear it
 * and review it, and that page comes back here with the filters as they were.
 *
 * Volunteers and coordinators see the same list, on the same tab, and a row
 * opens the detection under it. Only a coordinator's card column links to the
 * card's page.
 *
 * The filters live in the query string (/app/detections?species=Strix+varia),
 * so a reload or a link to a colleague shows the same list. The server filters,
 * sorts and pages; this only asks.
 *
 * It opens on the past DEFAULT_MONTHS months, with those dates filled into the
 * date fields rather than left blank: a list of detections is always bounded
 * (a date range is what keeps the query cheap), so the screen may as well say
 * what it is bounded to and let a reviewer widen it. Clearing “Heard from” hands
 * the bounding back to the server, which answers for a window of its own and
 * says which, printed above the list.
 *
 * The filter fields are rendered once and only the results are redrawn, so a
 * date half typed in keeps its focus while the list catches up.
 */
const PAGE_SIZE = 50;

/** How far back the list looks until the date fields say otherwise. */
const DEFAULT_MONTHS = 3;

const STATUSES = [
  { id: "", label: "All" },
  { id: "unreviewed", label: "Unreviewed" },
  { id: "confirmed", label: "Confirmed" },
  { id: "rejected", label: "Discarded" },
];

/** Minimum confidence choices, in percent; 0 is any. */
const CONFIDENCES = [0, 50, 70, 80, 90];

/** The columns that sort, and which way each starts. */
const SORTS = { heard: "desc", species: "asc", confidence: "desc" };

const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** A date as YYYY-MM-DD in the browser's timezone, which is what a date field takes. */
function dayOf(date) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

/** The dates the list starts on: the DEFAULT_MONTHS months up to today. */
function defaultDates(now = new Date()) {
  const from = new Date(now);
  from.setMonth(from.getMonth() - DEFAULT_MONTHS);
  return { from: dayOf(from), to: dayOf(now) };
}

/**
 * The list's view as the query string gives it, anything unreadable left at
 * its default. dates are the range a query string that names no readable ones
 * of its own gets, so a bare /app/detections is the past DEFAULT_MONTHS months.
 */
function viewFrom(params, dates) {
  const sort = Object.hasOwn(SORTS, params.get("sort")) ? params.get("sort") : "heard";
  const order = params.get("order") === "asc" || params.get("order") === "desc" ? params.get("order") : SORTS[sort];
  const min = Number(params.get("min"));
  const status = params.get("status") ?? "";
  return {
    status: STATUSES.some((s) => s.id === status) ? status : "",
    species: params.get("species") ?? "",
    min: CONFIDENCES.includes(min) ? min : 0,
    from: DATE.test(params.get("from")) ? params.get("from") : dates.from,
    to: DATE.test(params.get("to")) ? params.get("to") : dates.to,
    sort,
    order,
    page: Math.max(1, Number.parseInt(params.get("page"), 10) || 1),
  };
}

/** The query string for a view, leaving out what is at its default. */
function paramsFor(view) {
  const params = new URLSearchParams();
  if (view.status) params.set("status", view.status);
  if (view.species) params.set("species", view.species);
  if (view.min) params.set("min", view.min);
  if (view.from) params.set("from", view.from);
  if (view.to) params.set("to", view.to);
  if (view.sort !== "heard") params.set("sort", view.sort);
  if (view.order !== SORTS[view.sort]) params.set("order", view.order);
  if (view.page > 1) params.set("page", view.page);
  return params;
}

/** Midnight at the start of a YYYY-MM-DD day, in the browser's timezone. */
function startOfDay(date, addDays = 0) {
  const d = new Date(`${date}T00:00:00`);
  d.setDate(d.getDate() + addDays);
  return d;
}

class Detections extends BaseElement {
  static styles = [typography, controls, forms, tables];

  /** The range the list opens on, fixed when it opens so a long session doesn't drift over midnight. */
  #dates = defaultDates();
  #view = viewFrom(query(), this.#dates);
  /**
   * {status: loading|ready|error, detections, total, species, window, error};
   * a refresh keeps the last list showing. window is the date range the server
   * bounded the answer to because this view named none, or null.
   */
  #state = { status: "loading", detections: [], total: 0, species: [], window: null, error: null };
  #busy = false;
  /** Only the latest request's answer is shown. */
  #asked = 0;

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  get actions() {
    return {
      status: (el) => this.#change({ status: el.dataset.status }),
      sort: (el) => {
        const sort = el.dataset.sort;
        const order = sort === this.#view.sort ? (this.#view.order === "asc" ? "desc" : "asc") : SORTS[sort];
        this.#change({ sort, order });
      },
      page: (el) => this.#change({ page: Number(el.dataset.page) }),
      clear: () => {
        this.#change({ status: "", species: "", min: 0, ...this.#dates });
        // The fields show what they were set to; put them back.
        this.render();
      },
      species: (el) => this.#change({ species: el.value }),
      min: (el) => this.#change({ min: Number(el.value) }),
      from: (el) => this.#change({ from: el.value }),
      to: (el) => this.#change({ to: el.value }),
    };
  }

  /** Apply a change to the view. Anything but a page change starts again from the first page. */
  #change(changes) {
    this.#view = { ...this.#view, page: 1, ...changes };
    replaceQuery(paramsFor(this.#view));
    this.#load();
  }

  async #load() {
    const asked = ++this.#asked;
    const view = this.#view;
    const params = { sort: view.sort, order: view.order, limit: PAGE_SIZE, offset: (view.page - 1) * PAGE_SIZE };
    if (view.status) params.status = view.status;
    if (view.species) params.species = view.species;
    if (view.min) params.minConfidence = view.min / 100;
    if (view.from) params.since = startOfDay(view.from).toISOString();
    if (view.to) params.until = startOfDay(view.to, 1).toISOString();

    this.#busy = true;
    this.#update();
    let next;
    try {
      const { detections, total, species, window: bounded } = await api.fetchDetections(params);
      next = { status: "ready", detections, total, species, window: bounded ?? null, error: null };
    } catch (error) {
      next = { ...this.#state, status: this.#state.status === "loading" ? "error" : this.#state.status, error };
    }
    if (asked !== this.#asked) return;
    this.#busy = false;
    this.#state = next;

    // A page past the end (the list shrank, or an old link): go to the last one.
    const last = Math.max(1, Math.ceil(next.total / PAGE_SIZE));
    if (next.status === "ready" && view.page > last) return this.#change({ page: last });

    if (this.isConnected) this.#update();
  }

  render() {
    const v = this.#view;
    this.shadowRoot.innerHTML = `
      <style>
        [hidden] { display: none !important; }
        .filters { display: flex; align-items: center; gap: var(--bs-space-3); margin-bottom: var(--bs-space-4); flex-wrap: wrap; }
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

        .fields {
          display: grid;
          grid-template-columns: minmax(14rem, 2fr) repeat(3, minmax(9.5rem, 1fr)) auto;
          gap: var(--bs-space-3);
          align-items: end;
          margin-bottom: var(--bs-space-6);
        }
        @media (max-width: 900px) {
          .fields { grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr)); }
        }
        .fields .label { font-size: 0.78125rem; margin-bottom: 0.3125rem; color: var(--bs-text-body); }
        .fields .field { min-height: 2.625rem; font-size: 0.875rem; }
        .clear { padding: 0.625rem 0.875rem; font-size: 0.8125rem; }

        .results { transition: opacity 120ms ease; }
        .results[aria-busy="true"] { opacity: 0.55; }
        th { padding-top: 0; }
        td { padding-top: 0.8125rem; padding-bottom: 0.8125rem; font-size: 0.875rem; vertical-align: middle; }
        th .sort {
          border: 0;
          background: none;
          padding: 0;
          font: inherit;
          letter-spacing: inherit;
          text-transform: inherit;
          color: inherit;
          white-space: nowrap;
        }
        th .sort:hover, th[aria-sort] .sort { color: var(--bs-text); }
        th .arrow { display: inline-block; width: 1ch; }
        th.right, td.right { text-align: right; }
        .nowrap { white-space: nowrap; }

        /* The species links to the detection, and its link covers the row. */
        tbody tr { position: relative; }
        tbody tr:hover { background: var(--bs-surface-sunk); }
        /* On a narrow screen the table scrolls in its box rather than stacking a name a word a line. */
        .species { min-width: 13rem; }
        .station { min-width: 10rem; }
        .species a { color: var(--bs-text); text-decoration: none; }
        .species a::after { content: ""; position: absolute; inset: 0; }
        tbody tr:hover .species a { text-decoration: underline; }
        .sci { font-style: italic; color: var(--bs-text-muted); font-size: 0.8125rem; }
        .station { color: var(--bs-text-body); }
        .ref { font-family: var(--bs-font-mono); font-size: 0.78125rem; white-space: nowrap; }
        /* Above the species link, so the card opens the card. */
        .ref a { position: relative; z-index: 1; }

        .pager { display: flex; align-items: center; justify-content: space-between; gap: var(--bs-space-4); margin-top: var(--bs-space-5); flex-wrap: wrap; }
        .pager .row { display: flex; gap: var(--bs-space-2); }
        .pager .btn { padding: 0.5rem 0.875rem; font-size: 0.8125rem; }
        .quiet { font-size: 0.8125rem; color: var(--bs-text-muted); }
        .window { margin: calc(-1 * var(--bs-space-3)) 0 var(--bs-space-4); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
      </style>

      <div class="filters" role="group" aria-label="Review">
        ${STATUSES.map(
          (s) => `<button class="filter" data-action="status" data-status="${s.id}" aria-pressed="false">${s.label}</button>`,
        ).join("")}
        <span class="tally" aria-live="polite"></span>
      </div>

      <div class="fields">
        <label>
          <span class="label">Species</span>
          <select class="field" data-change="species"></select>
        </label>
        <label>
          <span class="label">Confidence</span>
          <select class="field" data-change="min">
            ${CONFIDENCES.map(
              (c) => `<option value="${c}" ${c === v.min ? "selected" : ""}>${c ? `${c}% or more` : "Any"}</option>`,
            ).join("")}
          </select>
        </label>
        <label>
          <span class="label">Heard from</span>
          <input class="field" type="date" data-change="from" value="${escapeHTML(v.from)}">
        </label>
        <label>
          <span class="label">Heard to</span>
          <input class="field" type="date" data-change="to" value="${escapeHTML(v.to)}">
        </label>
        <button type="button" class="btn btn--quiet clear" data-action="clear">Clear filters</button>
      </div>

      <p class="quiet window" hidden></p>
      <div class="results"></div>
    `;
    this.#update();
  }

  /** Redraws everything but the filter fields themselves. */
  #update() {
    const results = this.$(".results");
    if (!results) return;
    const v = this.#view;
    const { status, detections, total, species, window: bounded, error } = this.#state;
    // Filtered means narrowed from how the tab opens, so the range it opens on
    // doesn't make a program with nothing in it look like a filter nobody set.
    const filtered = Boolean(v.status || v.species || v.min) || v.from !== this.#dates.from || v.to !== this.#dates.to;
    // What the server bounded the answer to, said once above the list rather
    // than left for someone to notice an old detection missing.
    const note = this.$(".window");
    note.hidden = !(status === "ready" && bounded && total > 0);
    if (bounded) {
      note.textContent = `No “Heard from” date, so this shows the last ${bounded.days} days. Set one to look further back.`;
    }

    for (const pill of this.$$('[data-action="status"]')) {
      pill.setAttribute("aria-pressed", String(pill.dataset.status === v.status));
    }
    this.$(".clear").hidden = !filtered;
    this.$(".tally").textContent =
      status === "ready" ? `${count(total)} ${total === 1 ? "detection" : "detections"}` : "";
    this.#speciesOptions(species);
    results.setAttribute("aria-busy", String(this.#busy));

    if (status === "loading") {
      results.innerHTML = `<p class="empty">Loading detections…</p>`;
    } else if (status === "error") {
      results.innerHTML = `<p class="empty">Couldn't load the detections: ${escapeHTML(error.message)}</p>`;
    } else if (total === 0) {
      results.innerHTML = filtered
        ? `<p class="empty">No detections match those filters.</p>`
        : `<p class="empty">BirdNET hasn't heard anything in the last ${DEFAULT_MONTHS} months. Detections show up here as each card is analyzed; set “Heard from” to look further back.</p>`;
    } else {
      results.innerHTML = `
        ${error ? `<p class="error" role="alert">Couldn't refresh the list: ${escapeHTML(error.message)}</p>` : ""}
        <div class="table-scroll">
          <table>
            <thead>
              <tr>
                ${this.#sortHeader("heard", "Heard")}
                ${this.#sortHeader("species", "Species")}
                ${this.#sortHeader("confidence", "Confidence", "right")}
                <th scope="col">Station</th>
                <th scope="col">Card</th>
                <th scope="col">Review</th>
              </tr>
            </thead>
            <tbody>${detections.map((d) => this.#row(d)).join("")}</tbody>
          </table>
        </div>
        ${this.#pager(total)}
      `;
    }
    // A refresh that failed says so once; the next one clears it.
    if (status === "ready") this.#state.error = null;
  }

  /** The species select: every species in the list as filtered, and the one picked even if none are left. */
  #speciesOptions(species) {
    const select = this.$('select[data-change="species"]');
    const picked = this.#view.species;
    const options = species.map((s) => ({ value: s.scientificName, label: `${s.commonName} (${count(s.detections)})` }));
    if (picked && !species.some((s) => s.scientificName === picked)) options.unshift({ value: picked, label: `${picked} (0)` });
    select.innerHTML = `
      <option value="">All species</option>
      ${options
        .map((o) => `<option value="${escapeHTML(o.value)}" ${o.value === picked ? "selected" : ""}>${escapeHTML(o.label)}</option>`)
        .join("")}
    `;
  }

  #sortHeader(sort, label, align = "") {
    const v = this.#view;
    const active = v.sort === sort;
    const ariaSort = active ? `aria-sort="${v.order === "asc" ? "ascending" : "descending"}"` : "";
    const arrow = active ? (v.order === "asc" ? "↑" : "↓") : "";
    return `
      <th scope="col" class="${align}" ${ariaSort}>
        <button type="button" class="sort" data-action="sort" data-sort="${sort}">${label} <span class="arrow" aria-hidden="true">${arrow}</span></button>
      </th>`;
  }

  #row(d) {
    const { kind, label } = reviewChip(d.reviewStatus);
    const ref = encodeURIComponent(d.reference);
    const href = `${path()}/${ref}/${encodeURIComponent(d.id)}${location.search}`;
    return `
      <tr>
        <td class="nowrap">${escapeHTML(dateAtTime(d.detectedAt))}</td>
        <td class="species">
          <a href="${escapeHTML(href)}">${escapeHTML(d.commonName)}</a>
          ${d.scientificName && d.scientificName !== d.commonName ? `<span class="sci">${escapeHTML(d.scientificName)}</span>` : ""}
        </td>
        <td class="num">${Math.round(d.confidence * 100)}%</td>
        <td class="station">${escapeHTML(d.stationName)}</td>
        <td class="ref">${session.isAdmin() ? `<a href="/admin/uploads/${ref}">${escapeHTML(d.reference)}</a>` : escapeHTML(d.reference)}</td>
        <td><bs-chip kind="${kind}">${escapeHTML(label)}</bs-chip></td>
      </tr>
    `;
  }

  #pager(total) {
    const page = this.#view.page;
    const last = Math.ceil(total / PAGE_SIZE);
    const first = (page - 1) * PAGE_SIZE + 1;
    const shownTo = Math.min(page * PAGE_SIZE, total);
    if (last <= 1) return "";
    return `
      <nav class="pager" aria-label="Pages">
        <span class="quiet">${count(first)}–${count(shownTo)} of ${count(total)}</span>
        <span class="row">
          <button type="button" class="btn btn--quiet" data-action="page" data-page="${page - 1}" ${page <= 1 ? "disabled" : ""}>← Previous</button>
          <button type="button" class="btn btn--quiet" data-action="page" data-page="${page + 1}" ${page >= last ? "disabled" : ""}>Next →</button>
        </span>
      </nav>
    `;
  }
}

customElements.define("bs-detections", Detections);
