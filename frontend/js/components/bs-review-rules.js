import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { count, dateAtTime } from "../format.js";
import * as api from "../api.js";
import { SPECIES_LIST, fold, speciesIndex } from "../species-lookup.js";

/**
 * <bs-review-rules> -- the review rules (REVIEW.md): which species are
 * accepted on BirdNET's word and which a reviewer hears, each above a
 * confidence floor of its own. For an admin or birding expert.
 *
 * The table is every species BirdNET has heard lately and every species with
 * a rule, side by side, so a rule is set against what it will do: beside each
 * is what the last four weeks would have come to under the rule as drafted --
 * counted, sent to a reviewer, or below the floor -- and the totals of all of
 * them are above the table. A species with no rule of its own follows the
 * default; picking "Default" for one takes its rule away.
 *
 * Nothing is saved until Save, which stores the draft as the next version.
 * The server then routes every card in review by it again, in the background.
 * A table can be exported as JSON and imported again, and with no rules yet
 * the page offers the suggested one (/data/suggested-review-rules.json),
 * drafted from two weeks at Marymoor.
 *
 * "Add a species" looks either name up in BirdNET's species list as it is
 * typed, and fills in the other: a rule has to name a species as BirdNET
 * spells it to route anything. A name the list doesn't have is added as typed.
 */

/** How far back the counts beside the rules look. */
const HEARD_DAYS = 28;
/** The width of BirdNET's confidence bands the server counts in (byConfidence). */
const BAND = 0.05;
const SUGGESTED = "/data/suggested-review-rules.json";

class ReviewRules extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];

  #state = { status: "loading", error: null };
  /** The rules as stored, or null before the first save. */
  #stored = null;
  /** The defaults a first table starts from, from the server. */
  #suggested = null;
  /** What was heard lately, species by species, or null if that couldn't load. */
  #heard = null;
  #heardError = null;
  /**
   * The rules being edited: {defaults, boutGapMinutes, spotChecksPerSpecies,
   * species: Map of scientific name to rule}, or null when there are no rules
   * and none has been started.
   */
  #draft = null;
  #dirty = false;
  #search = "";
  #saving = false;
  #message = null; // {kind: "error" | "done", text}
  /** BirdNET's species list, fetched the first time a name is typed: a promise of its index, or of null. */
  #names = null;
  /** That index once it has loaded, for the moment a species is added. */
  #index = null;

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  async #load() {
    const [rules, heard] = await Promise.allSettled([api.fetchReviewRules(), api.fetchSpeciesHeard(HEARD_DAYS)]);
    if (rules.status === "rejected") {
      this.#state = { status: "error", error: rules.reason };
    } else {
      this.#stored = rules.value.rules;
      this.#suggested = rules.value.suggested;
      this.#draft = this.#stored ? draftOf(this.#stored) : null;
      this.#dirty = false;
      this.#state = { status: "ready", error: null };
    }
    if (heard.status === "fulfilled") this.#heard = heard.value.species;
    else this.#heardError = heard.reason;
    if (this.isConnected) this.render();
  }

  get actions() {
    return {
      suggest: async () => {
        try {
          const res = await fetch(SUGGESTED);
          if (!res.ok) throw new Error(`the suggested table didn't load (${res.status})`);
          this.#start(tableFrom(await res.json()));
        } catch (error) {
          this.#say("error", `Couldn't start from the suggested table: ${error.message}`);
        }
      },
      scratch: () => this.#start({ ...this.#suggested, species: new Map() }),
      import: () => this.$("#import-file").click(),
      importFile: async (el) => {
        const file = el.files?.[0];
        el.value = "";
        if (!file) return;
        try {
          this.#start(tableFrom(JSON.parse(await file.text())));
          this.#say("done", `Imported ${file.name}. Nothing is saved until you save it.`);
        } catch (error) {
          this.#say("error", `Couldn't import ${file.name}: ${error.message}`);
        }
      },
      export: () => this.#export(),
      discard: () => {
        this.#draft = this.#stored ? draftOf(this.#stored) : null;
        this.#dirty = false;
        this.#message = null;
        this.render();
      },
      save: () => this.#save(),

      search: (el) => {
        this.#search = el.value;
        this.#keepingFocus(() => this.render());
      },
      setting: (el) => {
        const value = Number(el.value);
        if (el.name === "defaultAction") this.#draft.defaults = { ...this.#draft.defaults, action: el.value };
        else if (el.name === "defaultFloor") this.#draft.defaults = { ...this.#draft.defaults, minConfidence: clampFloor(value) };
        else if (Number.isFinite(value)) this.#draft[el.name] = Math.round(value);
        this.#changed();
      },
      action: (el) => {
        const name = el.dataset.species;
        const rule = this.#draft.species.get(name);
        if (el.value === "default") this.#draft.species.delete(name);
        else
          this.#draft.species.set(name, {
            scientificName: name,
            commonName: el.dataset.common || name,
            minConfidence: this.#draft.defaults.minConfidence,
            ...rule,
            action: el.value,
          });
        this.#changed();
      },
      floor: (el) => {
        const rule = this.#draft.species.get(el.dataset.species);
        if (rule) rule.minConfidence = clampFloor(Number(el.value));
        this.#changed();
      },
      lookup: (el) => this.#lookUp(el),
      add: (el, event) => {
        event.preventDefault();
        const { scientificName, commonName } = this.#named(new FormData(el));
        if (!scientificName) return;
        if (!this.#draft.species.has(scientificName)) {
          this.#draft.species.set(scientificName, {
            scientificName,
            commonName,
            action: "review",
            minConfidence: this.#draft.defaults.minConfidence,
          });
        }
        this.#search = scientificName;
        this.#changed();
      },
    };
  }

  /** BirdNET's species list as an index, or null if it doesn't load; then the form works as typed. */
  #namesOf() {
    this.#names ??= fetch(SPECIES_LIST)
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(`${res.status}`))))
      .then((json) => (this.#index = speciesIndex(json.species ?? [])))
      .catch(() => null);
    return this.#names;
  }

  /**
   * As a name is typed into "Add a species": suggests BirdNET's names of the
   * kind the field holds, and when it holds one, fills in the other name --
   * unless that one was typed by hand. A name filled in is taken out again if
   * the one it came from stops matching.
   */
  async #lookUp(field) {
    const names = await this.#namesOf();
    const form = field.form;
    if (!names || !form?.isConnected) return;
    const { scientificName: scientific, commonName: common } = form.elements;
    const kind = field === scientific ? "scientific" : "common";
    const other = field === scientific ? common : scientific;
    delete field.dataset.filled;

    const list = this.shadowRoot.getElementById(field.getAttribute("list"));
    if (list) {
      list.innerHTML = names
        .suggest(field.value, kind)
        .map(({ scientificName, commonName }) =>
          kind === "scientific"
            ? `<option value="${escapeHTML(scientificName)}" label="${escapeHTML(commonName)}"></option>`
            : `<option value="${escapeHTML(commonName)}" label="${escapeHTML(scientificName)}"></option>`,
        )
        .join("");
    }

    const match = names.find(field.value, kind);
    if (match && (!other.value.trim() || other.dataset.filled)) {
      other.value = kind === "scientific" ? match.commonName : match.scientificName;
      other.dataset.filled = "true";
    } else if (!match && other.dataset.filled) {
      other.value = "";
      delete other.dataset.filled;
    }

    const hint = this.$("#add-hint");
    if (hint) hint.textContent = addHint(names, scientific.value);
  }

  /**
   * The species "Add a species" names: as BirdNET spells it when the list has
   * it -- a common name typed where the scientific one goes, too -- and as
   * typed when it doesn't. A common name typed by hand is kept.
   */
  #named(form) {
    const scientificName = String(form.get("scientificName") ?? "").trim().replace(/\s+/g, " ");
    const commonName = String(form.get("commonName") ?? "").trim().replace(/\s+/g, " ");
    const match = this.#index?.find(scientificName, "scientific") ?? this.#index?.find(scientificName, "common");
    if (!match) return { scientificName, commonName: commonName || scientificName };
    const typed = fold(commonName);
    return {
      scientificName: match.scientificName,
      commonName: !typed || typed === fold(match.commonName) || typed === fold(scientificName) ? match.commonName : commonName,
    };
  }

  #start(table) {
    this.#draft = table;
    this.#dirty = true;
    this.#message = null;
    this.render();
  }

  #changed() {
    this.#dirty = true;
    this.#message = null;
    this.#keepingFocus(() => this.render());
  }

  #say(kind, text) {
    this.#message = { kind, text };
    this.render();
  }

  /** Re-renders without losing the field being typed in. */
  #keepingFocus(draw) {
    const active = this.shadowRoot.activeElement;
    const id = active?.id;
    const caret = active?.selectionStart;
    draw();
    const again = id && this.shadowRoot.getElementById(id);
    if (!again) return;
    again.focus();
    if (caret != null && typeof again.setSelectionRange === "function") {
      try {
        again.setSelectionRange(caret, caret);
      } catch {
        // A number field has no caret to put back.
      }
    }
  }

  async #save() {
    if (this.#saving || !this.#draft) return;
    this.#saving = true;
    this.#message = null;
    this.render();
    try {
      const { rules } = await api.saveReviewRules({ version: this.#stored?.version ?? 0, ...bodyOf(this.#draft) });
      this.#stored = rules;
      this.#draft = draftOf(rules);
      this.#dirty = false;
      this.#message = {
        kind: "done",
        text: `Saved as version ${rules.version}. Every card in review is being routed by it now, in the background; a large card takes a few minutes.`,
      };
    } catch (error) {
      this.#message = { kind: "error", text: `Couldn't save the rules: ${error.message}` };
    } finally {
      this.#saving = false;
      if (this.isConnected) this.render();
    }
  }

  #export() {
    const table = this.#draft;
    if (!table) return;
    const version = this.#dirty ? "draft" : `v${this.#stored?.version ?? 0}`;
    const body = { exportedAt: new Date().toISOString(), version: this.#stored?.version ?? 0, ...bodyOf(table) };
    const url = URL.createObjectURL(new Blob([JSON.stringify(body, null, 2)], { type: "application/json" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `birdsense-review-rules-${version}.json`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  }

  /**
   * Every species heard lately and every species with a rule, the most heard
   * first: {scientificName, commonName, heard, rule}.
   */
  #rows() {
    const rows = new Map();
    for (const h of this.#heard ?? []) {
      rows.set(h.scientificName, { scientificName: h.scientificName, commonName: h.commonName, heard: h, rule: null });
    }
    for (const [name, rule] of this.#draft?.species ?? []) {
      const row = rows.get(name) ?? { scientificName: name, commonName: rule.commonName, heard: null, rule: null };
      row.rule = rule;
      rows.set(name, row);
    }
    return [...rows.values()].sort(
      (a, b) => (b.heard?.detections ?? -1) - (a.heard?.detections ?? -1) || a.commonName.localeCompare(b.commonName),
    );
  }

  /** What a species' detections come to under a rule: {counted, review, below}. */
  #outcome(heard, rule) {
    const total = heard?.detections ?? 0;
    if (!this.#draft) return { counted: 0, review: total, below: 0 };
    const { action, minConfidence } = rule ?? this.#draft.defaults;
    const kept = atOrAbove(heard?.byConfidence, minConfidence);
    return {
      counted: action === "accept" ? kept : 0,
      review: action === "review" ? kept : 0,
      below: total - kept,
    };
  }

  render() {
    const { status, error } = this.#state;
    if (status !== "ready") {
      this.shadowRoot.innerHTML = `
        <h1>Review rules</h1>
        <p class="note">${status === "loading" ? "Loading the rules…" : `Couldn't load the rules: ${escapeHTML(error.message)}`}</p>`;
      return;
    }
    const rows = this.#rows();
    const totals = rows.reduce(
      (t, row) => {
        const o = this.#outcome(row.heard, row.rule);
        return { counted: t.counted + o.counted, review: t.review + o.review, below: t.below + o.below };
      },
      { counted: 0, review: 0, below: 0 },
    );
    const needle = this.#search.trim().toLowerCase();
    const shown = needle
      ? rows.filter((r) => r.commonName.toLowerCase().includes(needle) || r.scientificName.toLowerCase().includes(needle))
      : rows;

    this.shadowRoot.innerHTML = `
      <style>
        h1 { margin-bottom: var(--bs-space-3); }
        .lede { max-width: 46rem; margin-bottom: var(--bs-space-5); }
        .bar { display: flex; flex-wrap: wrap; gap: var(--bs-space-2); align-items: center; margin-bottom: var(--bs-space-5); }
        .bar .state { margin-right: auto; font-size: 0.875rem; color: var(--bs-text-muted); }
        .message { margin-bottom: var(--bs-space-5); font-size: 0.875rem; }
        .done { color: var(--bs-text-body); background: var(--bs-surface-sunk); border: 1px solid var(--bs-border); padding: var(--bs-space-2) var(--bs-space-3); }
        .start { margin-bottom: var(--bs-space-6); max-width: 46rem; }
        .start p { margin-bottom: var(--bs-space-4); }
        .start .row { display: flex; flex-wrap: wrap; gap: var(--bs-space-2); }
        .settings { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr)); gap: var(--bs-space-4); margin-bottom: var(--bs-space-5); }
        .settings .hint { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.375rem; }
        .totals { display: flex; flex-wrap: wrap; gap: var(--bs-space-5); margin-bottom: var(--bs-space-4); font-size: 0.875rem; }
        .totals strong { font-family: var(--bs-font-mono); font-weight: 500; font-size: 1.125rem; display: block; }
        .find { display: flex; flex-wrap: wrap; gap: var(--bs-space-3); align-items: start; margin-bottom: var(--bs-space-3); }
        .find .search { flex: 1 1 14rem; }
        .find form { display: flex; flex-wrap: wrap; gap: var(--bs-space-2); align-items: end; }
        .add { display: grid; }
        /* As wide as the form above it, however long the hint. */
        .add .hint { width: 0; min-width: 100%; font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.375rem; }
        .field--row { min-height: 2.25rem; font-size: 0.875rem; }
        .name { font-size: 0.9375rem; }
        .latin { font-size: 0.78125rem; font-style: italic; color: var(--bs-text-muted); }
        .rule-note { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.125rem; }
        td { padding-top: 0.625rem; padding-bottom: 0.625rem; }
        td select { min-width: 8.5rem; }
        td input[type="number"] { width: 5.5rem; }
        .muted { color: var(--bs-text-muted); }
        .num { white-space: nowrap; }
        th.num { font-family: inherit; font-size: inherit; }
      </style>

      <h1>Review rules</h1>
      <p class="lede note">
        Each BirdNET detection is <strong>accepted</strong> on BirdNET's word if its species' rule says so,
        <strong>sent to a reviewer</strong> if it says review, and left out — kept, but neither counted nor
        heard — below its species' confidence floor. A species with no rule of its own follows the default.
        Beside each species is what the last ${HEARD_DAYS} days come to under the rule as it stands here.
      </p>

      ${this.#message ? `<p class="message ${this.#message.kind === "error" ? "error" : "done"}" role="${this.#message.kind === "error" ? "alert" : "status"}">${escapeHTML(this.#message.text)}</p>` : ""}
      <input id="import-file" type="file" accept="application/json,.json" hidden data-change="importFile" />

      ${this.#draft ? this.#editor(shown, totals) : this.#empty()}
    `;
  }

  #empty() {
    return `
      <section class="panel start">
        <h3>There are no review rules yet</h3>
        <p class="note">
          So nothing is routed: every detection waits for a reviewer, as it always has. Start from the
          suggested table — drafted from two weeks at Marymoor, with the birds common there accepted and
          every owl reviewed — or import one, then save it.
        </p>
        <div class="row">
          <button type="button" class="btn btn--navy btn--small" data-action="suggest">Start from the suggested table</button>
          <button type="button" class="btn btn--quiet btn--small" data-action="import">Import a table</button>
          <button type="button" class="btn btn--quiet btn--small" data-action="scratch">Start from scratch</button>
        </div>
      </section>
      ${this.#heardTable()}
    `;
  }

  /** With no rules, what was heard lately, so there is something to judge a table against. */
  #heardTable() {
    if (!this.#heard?.length) return "";
    return `
      <h3>Heard in the last ${HEARD_DAYS} days</h3>
      <div class="table-scroll">
        <table>
          <thead><tr><th scope="col">Species</th><th scope="col" class="num">Detections</th><th scope="col" class="num">Days</th></tr></thead>
          <tbody>${this.#heard
            .map(
              (h) => `<tr>
                <td><div class="name">${escapeHTML(h.commonName)}</div><div class="latin">${escapeHTML(h.scientificName)}</div></td>
                <td class="num">${count(h.detections)}</td>
                <td class="num">${count(h.days)}</td>
              </tr>`,
            )
            .join("")}</tbody>
        </table>
      </div>`;
  }

  #editor(rows, totals) {
    const d = this.#draft;
    const stored = this.#stored;
    const state = !stored
      ? "Not saved yet."
      : // dateAtTime ends in "a.m." or "p.m.", which is already the full stop.
        `Version ${stored.version}${stored.updatedBy ? `, saved by ${escapeHTML(stored.updatedBy)}` : ""}${stored.updatedAt ? ` on ${escapeHTML(dateAtTime(stored.updatedAt))}` : ""}${this.#dirty ? ", with changes not saved yet." : stored.updatedAt ? "" : "."}`;
    return `
      <div class="bar">
        <span class="state">${state}</span>
        <button type="button" class="btn btn--quiet btn--small" data-action="import">Import</button>
        <button type="button" class="btn btn--quiet btn--small" data-action="export">Export</button>
        ${this.#dirty ? `<button type="button" class="btn btn--quiet btn--small" data-action="discard" ${this.#saving ? "disabled" : ""}>Discard changes</button>` : ""}
        <button type="button" class="btn btn--navy btn--small" data-action="save" ${!this.#dirty || this.#saving ? "disabled" : ""}>
          ${this.#saving ? "Saving…" : "Save"}
        </button>
      </div>

      <div class="settings">
        <div>
          <label class="label" for="default-action">Default, for a species with no rule</label>
          <select class="field field--sunk" id="default-action" name="defaultAction" data-change="setting">
            ${actionOptions(d.defaults.action)}
          </select>
        </div>
        <div>
          <label class="label" for="default-floor">Its confidence floor</label>
          <input class="field field--sunk" id="default-floor" name="defaultFloor" type="number" min="0" max="1" step="${BAND}"
                 value="${d.defaults.minConfidence}" data-change="setting" />
        </div>
        <div>
          <label class="label" for="bout-gap">Bout gap, minutes</label>
          <input class="field field--sunk" id="bout-gap" name="boutGapMinutes" type="number" min="1" max="240" step="1"
                 value="${d.boutGapMinutes}" data-change="setting" />
          <p class="hint">How long a species may go unheard and still be one bout: what a reviewer hears as one, a verdict standing for all of it.</p>
        </div>
        <div>
          <label class="label" for="spot-checks">Spot-checks per species</label>
          <input class="field field--sunk" id="spot-checks" name="spotChecksPerSpecies" type="number" min="0" max="20" step="1"
                 value="${d.spotChecksPerSpecies}" data-change="setting" />
          <p class="hint">Bouts of each accepted species heard anyway, per card, to check the rule.</p>
        </div>
      </div>

      ${
        this.#heard
          ? `<div class="totals" aria-label="The last ${HEARD_DAYS} days under these rules">
               <div><strong>${count(totals.counted)}</strong>counted, accepted unheard</div>
               <div><strong>${count(totals.review)}</strong>sent to a reviewer</div>
               <div><strong>${count(totals.below)}</strong>below their floors</div>
               <div class="muted">the last ${HEARD_DAYS} days' detections, under the rules as they stand here</div>
             </div>`
          : `<p class="note">Couldn't load what was heard lately${this.#heardError ? `: ${escapeHTML(this.#heardError.message)}` : ""}.</p>`
      }

      <div class="find">
        <div class="search">
          <label class="label" for="rules-search">Find a species</label>
          <input class="field field--sunk field--row" id="rules-search" type="search" value="${escapeHTML(this.#search)}"
                 placeholder="Common or scientific name" data-input="search" />
        </div>
        <div class="add">
          <form data-submit="add" aria-label="Add a rule for a species not heard lately">
            <div>
              <label class="label" for="add-scientific">Add a species<span class="visually-hidden">: scientific name</span></label>
              <input class="field field--sunk field--row" id="add-scientific" name="scientificName" placeholder="Strix varia" required
                     autocomplete="off" list="add-scientific-names" aria-describedby="add-hint" data-input="lookup" />
              <datalist id="add-scientific-names"></datalist>
            </div>
            <div>
              <label class="label" for="add-common"><span class="visually-hidden">Common name</span>&nbsp;</label>
              <input class="field field--sunk field--row" id="add-common" name="commonName" placeholder="Barred Owl"
                     autocomplete="off" list="add-common-names" aria-describedby="add-hint" data-input="lookup" />
              <datalist id="add-common-names"></datalist>
            </div>
            <button class="btn btn--quiet btn--tiny">Add</button>
          </form>
          <p class="hint" id="add-hint" aria-live="polite">${escapeHTML(addHint(this.#index, ""))}</p>
        </div>
      </div>

      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Species</th>
              <th scope="col">Rule</th>
              <th scope="col">Floor</th>
              <th scope="col" class="num">Heard</th>
              <th scope="col" class="num">Counted</th>
              <th scope="col" class="num">To review</th>
              <th scope="col" class="num">Below floor</th>
              <th scope="col" class="num">Reviewed</th>
            </tr>
          </thead>
          <tbody>${rows.map((row, i) => this.#row(row, i)).join("")}</tbody>
        </table>
      </div>
      ${rows.length === 0 ? `<p class="note">No species match.</p>` : ""}
      <p class="note">Counts go by BirdNET's confidence in steps of ${BAND}, so a floor between two steps is counted at the step above it.</p>
    `;
  }

  #row(row, i) {
    const { heard, rule } = row;
    const d = this.#draft;
    const o = this.#outcome(heard, rule);
    const name = escapeHTML(row.scientificName);
    const reviewed = heard ? heard.confirmed + heard.rejected : 0;
    const dash = `<span class="muted">–</span>`;
    return `
      <tr>
        <td>
          <div class="name">${escapeHTML(row.commonName)}</div>
          <div class="latin">${name}</div>
          ${rule?.note ? `<div class="rule-note">${escapeHTML(rule.note)}</div>` : ""}
        </td>
        <td>
          <label class="visually-hidden" for="action-${i}">Rule for ${escapeHTML(row.commonName)}</label>
          <select class="field field--sunk field--row" id="action-${i}" data-change="action"
                  data-species="${name}" data-common="${escapeHTML(row.commonName)}">
            <option value="default" ${rule ? "" : "selected"}>Default (${d.defaults.action})</option>
            ${actionOptions(rule?.action ?? "")}
          </select>
        </td>
        <td>
          <label class="visually-hidden" for="floor-${i}">Confidence floor for ${escapeHTML(row.commonName)}</label>
          <input class="field field--sunk field--row" id="floor-${i}" type="number" min="0" max="1" step="${BAND}"
                 value="${rule ? rule.minConfidence : d.defaults.minConfidence}" data-change="floor" data-species="${name}"
                 ${rule ? "" : "disabled"} />
        </td>
        <td class="num">${heard ? `${count(heard.detections)}<div class="muted">${count(heard.days)} ${heard.days === 1 ? "day" : "days"}</div>` : dash}</td>
        <td class="num">${heard ? count(o.counted) : dash}</td>
        <td class="num">${heard ? count(o.review) : dash}</td>
        <td class="num">${heard ? count(o.below) : dash}</td>
        <td class="num">${reviewed ? `${count(heard.confirmed)} ✓ ${count(heard.rejected)} ✗` : dash}</td>
      </tr>`;
  }
}

/** What "Add a species" says under its fields, given the scientific name typed so far. */
function addHint(names, typed) {
  const general = "Type either name, and BirdNET's list fills in the other.";
  if (!names || !fold(typed)) return general;
  if (names.find(typed, "scientific")) return general;
  const common = names.find(typed, "common");
  if (common) return `${common.commonName} is a common name: the rule will be for ${common.scientificName}.`;
  if (names.suggest(typed, "scientific").length) return general;
  return (
    "BirdNET has no species by that scientific name, so a rule for it would route nothing. " +
    "It spells some names its own way: Corthylio calendula, not Regulus calendula."
  );
}

/** The two actions as options, with selected chosen. */
function actionOptions(selected) {
  return [
    ["accept", "Accept"],
    ["review", "Review"],
  ]
    .map(([value, label]) => `<option value="${value}" ${selected === value ? "selected" : ""}>${label}</option>`)
    .join("");
}

/** How many of a species' detections are at or above a floor, from its confidence bands. */
function atOrAbove(bands, floor) {
  if (!bands) return 0;
  // A floor between two band edges counts the band it falls in as below it.
  const first = Math.ceil(floor / BAND - 1e-9);
  return bands.slice(Math.max(first, 0)).reduce((sum, n) => sum + n, 0);
}

const clampFloor = (value) => (Number.isFinite(value) ? Math.min(Math.max(value, 0), 1) : 0);

/** Stored rules as a draft to edit. */
function draftOf(rules) {
  return {
    defaults: { ...rules.defaults },
    boutGapMinutes: rules.boutGapMinutes,
    spotChecksPerSpecies: rules.spotChecksPerSpecies,
    species: new Map(rules.species.map((s) => [s.scientificName, { ...s }])),
  };
}

/** A draft as the API and an exported file have it. */
function bodyOf(draft) {
  return {
    defaults: draft.defaults,
    boutGapMinutes: draft.boutGapMinutes,
    spotChecksPerSpecies: draft.spotChecksPerSpecies,
    species: [...draft.species.values()].sort((a, b) => a.scientificName.localeCompare(b.scientificName)),
  };
}

/**
 * A table read from a file -- an export, or the suggested one -- as a draft.
 * The server checks it properly on save; this only refuses what isn't a table
 * at all, and says which entry is wrong.
 */
function tableFrom(json) {
  if (!json || typeof json !== "object" || !Array.isArray(json.species)) {
    throw new Error("it isn't a table of review rules (it has no species list)");
  }
  const action = (a, where) => {
    if (a !== "accept" && a !== "review") throw new Error(`${where}: the action must be accept or review`);
    return a;
  };
  const floor = (f, where) => {
    if (typeof f !== "number" || f < 0 || f > 1) throw new Error(`${where}: the confidence floor must be from 0 to 1`);
    return f;
  };
  const defaults = json.defaults ?? { action: "review", minConfidence: 0.5 };
  const species = new Map();
  json.species.forEach((s, i) => {
    const name = String(s?.scientificName ?? "").trim();
    if (!name) throw new Error(`species ${i + 1} has no scientific name`);
    species.set(name, {
      scientificName: name,
      commonName: String(s.commonName ?? "").trim() || name,
      action: action(s.action, name),
      minConfidence: floor(s.minConfidence, name),
      ...(s.note ? { note: String(s.note) } : {}),
      ...(s.source ? { source: String(s.source) } : {}),
    });
  });
  return {
    defaults: { action: action(defaults.action, "the default"), minConfidence: floor(defaults.minConfidence, "the default") },
    boutGapMinutes: Number.isInteger(json.boutGapMinutes) ? json.boutGapMinutes : 15,
    spotChecksPerSpecies: Number.isInteger(json.spotChecksPerSpecies) ? json.spotChecksPerSpecies : 3,
    species,
  };
}

customElements.define("bs-review-rules", ReviewRules);
