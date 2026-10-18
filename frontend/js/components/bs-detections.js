import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, filters, forms, tables, typography } from "../shared-styles.js";
import { count, dateAtTime } from "../format.js";
import { reviewChip } from "../upload-status.js";
import { path, query, replaceQuery } from "../router.js";
import {
  CONFIDENCES,
  DEFAULT_DAYS,
  PAGE_SIZE,
  SORTS,
  STATUSES,
  defaultDates,
  exportView,
  loadPage,
  paramsFor,
  viewFrom,
} from "../detection-list.js";
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
 * sorts and pages; this only asks. What it asks for, and the page it gets
 * back, live in detection-list.js, so the detection page can step through this
 * same list without asking for it again.
 *
 * It opens on the past DEFAULT_DAYS days, with those dates filled into the
 * date fields rather than left blank: a list of detections is always bounded
 * (a date range is what keeps the query cheap), so the screen may as well say
 * what it is bounded to and let a reviewer widen it. Clearing “Heard from” hands
 * the bounding back to the server, which answers for a window of its own and
 * says which, printed above the list.
 *
 * The filter fields are rendered once and only the results are redrawn, so a
 * date half typed in keeps its focus while the list catches up.
 *
 * A card's page links here narrowed to that card (?card=OWL-20260914-SR03),
 * and, for a coordinator, to Perch's detections on it (&model=perch): this
 * is the one place detections are listed, so a card's are looked at with the
 * same filters, sorts and stepping as everything else. A card bounds the list
 * by itself, so it opens on the whole card rather than the past week, and a
 * line above the filters says what it is narrowed to and undoes it.
 *
 * “Export as JSON” saves every detection the list holds, not the page shown:
 * the same filters and order, asked of the server whole (GET
 * /detections/export), so a list of 11,201 is a file of 11,201.
 */

class Detections extends BaseElement {
  static styles = [typography, controls, forms, tables, filters];

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
  /** An export is being fetched; one at a time. */
  #exporting = false;
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
      // Back to how the list opened, keeping the card it is narrowed to.
      clear: () => {
        this.#change({ status: "", species: "", min: 0, ...this.#opening });
        // The fields show what they were set to; put them back.
        this.render();
      },
      // Every card's again, BirdNET's, over the week the tab opens on.
      every: () => {
        this.#change({ card: "", model: "", ...this.#dates });
        this.render();
      },
      birdnet: () => {
        this.#change({ model: "" });
        this.render();
      },
      species: (el) => this.#change({ species: el.value }),
      min: (el) => this.#change({ min: Number(el.value) }),
      from: (el) => this.#change({ from: el.value }),
      to: (el) => this.#change({ to: el.value }),
      export: () => this.#export(),
    };
  }

  /** The dates the list opens on: the past week, or for one card, none -- the whole card. */
  get #opening() {
    return this.#view.card ? { from: "", to: "" } : this.#dates;
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

    this.#busy = true;
    this.#update();
    let next;
    try {
      // The page is held by detection-list.js as well as shown here, so
      // Previous and Next on a detection opened from it cost nothing.
      const { detections, total, species, window: bounded } = await loadPage(view);
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

  /** Save every detection in the view as it stands as a JSON file. */
  async #export() {
    if (this.#exporting) return;
    this.#exporting = true;
    const problem = this.$(".export-error");
    problem.hidden = true;
    this.#update();
    try {
      const { blob, name } = await exportView(this.#view);
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = name;
      link.click();
      // The click only starts the save; let it take the blob before letting go.
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch (error) {
      problem.textContent = `Couldn't export the detections: ${error.message}`;
      problem.hidden = false;
    } finally {
      this.#exporting = false;
      if (this.isConnected) this.#update();
    }
  }

  render() {
    const v = this.#view;
    this.shadowRoot.innerHTML = `
      <style>
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
        .export-error { margin: calc(-1 * var(--bs-space-2)) 0 var(--bs-space-4); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        h1 { margin-bottom: var(--bs-space-5); }
        .scope { margin: calc(-1 * var(--bs-space-3)) 0 var(--bs-space-5); font-size: 0.90625rem; color: var(--bs-text-body); }
        .scope .ref { font-size: 0.84375rem; }
        .scope .btn { margin-left: var(--bs-space-2); }
      </style>

      <h1>Detections</h1>
      ${this.#scope()}

      <div class="filters" role="group" aria-label="Review">
        ${STATUSES.map(
          (s) => `<button class="filter" data-action="status" data-status="${s.id}" aria-pressed="false">${s.label}</button>`,
        ).join("")}
        <span class="tally" aria-live="polite"></span>
        <button type="button" class="btn btn--quiet btn--tiny export" data-action="export">Export as JSON</button>
      </div>
      <p class="error export-error" role="alert" hidden></p>

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
    const opening = this.#opening;
    const filtered = Boolean(v.status || v.species || v.min) || v.from !== opening.from || v.to !== opening.to;
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
    // What it saves is the list as last shown, so only once there is one.
    const exporter = this.$(".export");
    exporter.disabled = this.#exporting || this.#busy || status !== "ready" || total === 0;
    exporter.textContent = this.#exporting ? "Exporting…" : "Export as JSON";
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
        : v.card
          ? `<p class="empty">${v.model === "perch" ? "Perch" : "BirdNET"} hasn't heard anything on ${escapeHTML(v.card)}. Its detections show up here as each of its files is analyzed.</p>`
          : `<p class="empty">BirdNET hasn't heard anything in the last ${DEFAULT_DAYS} days. Detections show up here as each card is analyzed; set “Heard from” to look further back.</p>`;
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

  /**
   * What the list is narrowed to beyond its filters -- one card, or Perch's
   * detections rather than BirdNET's -- with the way out of each, or "".
   */
  #scope() {
    const { card, model } = this.#view;
    if (!card && !model) return "";
    const ref = card
      ? session.isAdmin()
        ? `<a class="ref" href="/admin/uploads/${encodeURIComponent(card)}">${escapeHTML(card)}</a>`
        : `<span class="ref">${escapeHTML(card)}</span>`
      : "";
    const what = model === "perch" ? "Perch's detections, a second opinion," : "Detections";
    return `
      <p class="scope">
        ${what} ${card ? `on card ${ref}` : "on every card"}.
        ${model ? `<button type="button" class="btn btn--quiet btn--tiny" data-action="birdnet">Show BirdNET's</button>` : ""}
        ${card ? `<button type="button" class="btn btn--quiet btn--tiny" data-action="every">Show every card</button>` : ""}
      </p>`;
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
