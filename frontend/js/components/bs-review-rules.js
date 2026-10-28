import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, filters, forms, panels, tables, typography } from "../shared-styles.js";
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
 * Until then every rule the draft adds, changes or takes away is marked in
 * the table, against the rules as saved (or, before the first save, the table
 * the draft started from), and "Only changes" narrows the table to them.
 * A table can be exported as JSON and imported again, and with no rules yet
 * the page offers the suggested one (/data/suggested-review-rules.json),
 * drafted from two weeks at Marymoor.
 *
 * A rule is added, or one changed, in its own panel above the table: a
 * species, its rule and floor, and a button that puts it into the draft. The
 * panel looks either name up in BirdNET's species list as it is typed, and
 * fills in the other: a rule has to name a species as BirdNET spells it to
 * route anything. A name the list doesn't have is added as typed.
 */

/** How far back the counts beside the rules look. */
const HEARD_DAYS = 28;
/** The width of BirdNET's confidence bands the server counts in (byConfidence). */
const BAND = 0.05;
const SUGGESTED = "/data/suggested-review-rules.json";
/** What the table says of a rule the draft adds, changes or takes away. */
const CHANGE_LABELS = { new: "New", changed: "Changed", removed: "Rule removed" };

class ReviewRules extends BaseElement {
  static styles = [typography, controls, forms, filters, panels, tables];

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
  /** What the draft's changes are counted against, in the draft's shape: the stored rules, or the table it started from. */
  #baseline = null;
  #dirty = false;
  #search = "";
  #onlyChanges = false;
  #saving = false;
  #message = null; // {kind: "error" | "done", text}
  /** What the add panel last put into the draft: {scientificName, text}. */
  #added = null;
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
      this.#baseline = this.#stored ? draftOf(this.#stored) : null;
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
        this.#baseline = this.#stored ? draftOf(this.#stored) : null;
        this.#dirty = false;
        this.#onlyChanges = false;
        this.#added = null;
        this.#message = null;
        this.render();
      },
      save: () => this.#save(),

      search: (el) => {
        this.#search = el.value;
        this.#keepingFocus(() => this.render());
      },
      onlyChanges: () => {
        this.#onlyChanges = !this.#onlyChanges;
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
            minConfidence: this.#baseline?.species.get(name)?.minConfidence ?? this.#draft.defaults.minConfidence,
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
      // The add panel's rule and floor, once someone has picked them, are
      // theirs: a species typed after that doesn't put its own rule in them.
      touch: (el) => {
        el.dataset.touched = "true";
      },
      add: (el, event) => {
        event.preventDefault();
        this.#add(el);
      },
      clearAdd: () => {
        const form = this.$("#add-form");
        if (form) this.#resetAdder(form);
        this.#added = null;
        this.#keepingFocus(() => this.render());
        this.$("#add-scientific")?.focus();
      },
      showAdded: () => {
        const name = this.#added?.scientificName;
        if (!name) return;
        // Whatever narrows the table might be hiding it.
        this.#search = "";
        this.render();
        const row = this.shadowRoot.querySelector(`tr[data-species="${CSS.escape(name)}"]`);
        row?.scrollIntoView({ block: "center" });
        row?.querySelector("select")?.focus({ preventScroll: true });
      },
    };
  }

  #start(table) {
    this.#draft = table;
    // Before the first save there is nothing stored to count changes against,
    // so they are counted against the table the draft started from.
    if (!this.#stored) this.#baseline = copyOf(table);
    this.#dirty = true;
    this.#onlyChanges = false;
    this.#added = null;
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

  /**
   * Re-renders without losing the field being typed in, or what is typed in
   * the add panel: every render rewrites the whole page, and a rule half
   * entered there shouldn't go because a floor changed in the table.
   */
  #keepingFocus(draw) {
    const active = this.shadowRoot.activeElement;
    const id = active?.id;
    const caret = active?.selectionStart;
    const adder = [...(this.$("#add-form")?.elements ?? [])]
      .filter((el) => el.id && "value" in el)
      .map((el) => ({ id: el.id, value: el.value, dataset: { ...el.dataset } }));
    draw();
    for (const { id: field, value, dataset } of adder) {
      const el = this.shadowRoot.getElementById(field);
      if (!el) continue;
      el.value = value;
      Object.assign(el.dataset, dataset);
    }
    if (adder.length) this.#describe(this.$("#add-form"));
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
    this.#keepingFocus(() => this.render());
    try {
      const { rules } = await api.saveReviewRules({ version: this.#stored?.version ?? 0, ...bodyOf(this.#draft) });
      this.#stored = rules;
      this.#draft = draftOf(rules);
      this.#baseline = draftOf(rules);
      this.#dirty = false;
      // Saved, the whole table is what there is to see again.
      this.#search = "";
      this.#onlyChanges = false;
      this.#added = null;
      this.#message = {
        kind: "done",
        text: `Saved as version ${rules.version}. Every card in review is being routed by it now, in the background; a large card takes a few minutes.`,
      };
    } catch (error) {
      this.#message = { kind: "error", text: `Couldn't save the rules: ${error.message}` };
    } finally {
      this.#saving = false;
      if (this.isConnected) this.#keepingFocus(() => this.render());
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

  /** BirdNET's species list as an index, or null if it doesn't load; then the form works as typed. */
  #namesOf() {
    this.#names ??= fetch(SPECIES_LIST)
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(`${res.status}`))))
      .then((json) => (this.#index = speciesIndex(json.species ?? [])))
      .catch(() => null);
    return this.#names;
  }

  /**
   * As a name is typed into the add panel: suggests BirdNET's names of the
   * kind the field holds, and when it holds one, fills in the other name --
   * unless that one was typed by hand. A name filled in is taken out again if
   * the one it came from stops matching.
   */
  async #lookUp(field) {
    const form = field.form;
    delete field.dataset.filled;
    this.#describe(form);
    const names = await this.#namesOf();
    if (!names || !form?.isConnected) return;
    const { scientificName: scientific, commonName: common } = form.elements;
    const kind = field === scientific ? "scientific" : "common";
    const other = field === scientific ? common : scientific;

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
    this.#describe(form);
  }

  /**
   * Brings the add panel up to date with the species typed in it: its rule
   * and floor show the species' rule in the draft (or the default) until
   * someone picks their own, the button says whether it adds a rule or
   * changes one, and the line under the fields says what is known of it.
   */
  #describe(form) {
    if (!form || !this.#draft) return;
    const { scientificName: scientificField, action, minConfidence, confirm } = form.elements;
    const { scientificName } = this.#named(new FormData(form));
    const rule = scientificName ? this.#draft.species.get(scientificName) : null;
    if (!action.dataset.touched) action.value = rule?.action ?? "review";
    if (!minConfidence.dataset.touched) minConfidence.value = String(rule?.minConfidence ?? this.#draft.defaults.minConfidence);
    confirm.textContent = rule ? "Change its rule" : "Add to the rules";

    const heard = scientificName && this.#heard?.find((h) => h.scientificName === scientificName);
    const about = !scientificName
      ? ""
      : `${rule ? `${rule.commonName} has a rule: ${ruleText(rule)}.` : ""}${
          this.#heard
            ? ` ${heard ? `Heard ${count(heard.detections)} ${heard.detections === 1 ? "time" : "times"} on ${count(heard.days)} ${heard.days === 1 ? "day" : "days"}` : "Not heard"} in the last ${HEARD_DAYS} days.`
            : ""
        }`;
    const hint = this.$("#add-hint");
    if (hint) hint.textContent = [addHint(this.#index, scientificField.value), about.trim()].filter(Boolean).join(" ");
  }

  /**
   * The species the add panel names: as BirdNET spells it when the list has
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

  /** Puts the add panel's rule into the draft, where the table marks it, and empties the panel for the next. */
  #add(form) {
    const data = new FormData(form);
    const { scientificName, commonName } = this.#named(data);
    if (!scientificName) return;
    const before = this.#draft.species.get(scientificName);
    const rule = {
      ...before,
      scientificName,
      commonName,
      action: data.get("action") === "accept" ? "accept" : "review",
      minConfidence: clampFloor(Number(data.get("minConfidence"))),
    };
    this.#draft.species.set(scientificName, rule);
    const same = before && before.action === rule.action && before.minConfidence === rule.minConfidence;
    this.#added = {
      scientificName,
      text: same
        ? `${commonName} already had that rule: ${ruleText(rule)}.`
        : `${before ? "Changed" : "Added"} ${commonName}: ${ruleText(rule)}. It is marked in the table below; Save puts it into effect.`,
    };
    this.#resetAdder(form);
    this.#changed();
  }

  #resetAdder(form) {
    form.reset();
    for (const el of form.elements) {
      delete el.dataset.filled;
      delete el.dataset.touched;
    }
    for (const list of form.querySelectorAll("datalist")) list.replaceChildren();
  }

  /** How a species' rule in the draft differs from the baseline: "new", "changed", "removed", or null. */
  #change(name) {
    if (!this.#baseline) return null;
    const before = this.#baseline.species.get(name);
    const now = this.#draft?.species.get(name);
    if (now && !before) return "new";
    if (before && !now) return "removed";
    if (now && (now.action !== before.action || now.minConfidence !== before.minConfidence)) return "changed";
    return null;
  }

  /** The settings the draft has changed: a Set of their field names. */
  #settingsChanged() {
    const changed = new Set();
    const [d, b] = [this.#draft, this.#baseline];
    if (!d || !b) return changed;
    if (d.defaults.action !== b.defaults.action) changed.add("defaultAction");
    if (d.defaults.minConfidence !== b.defaults.minConfidence) changed.add("defaultFloor");
    if (d.boutGapMinutes !== b.boutGapMinutes) changed.add("boutGapMinutes");
    if (d.spotChecksPerSpecies !== b.spotChecksPerSpecies) changed.add("spotChecksPerSpecies");
    return changed;
  }

  /**
   * Every species heard lately and every species with a rule -- in the draft,
   * or in the baseline, so a rule taken away still shows -- the most heard
   * first: {scientificName, commonName, heard, rule, change}.
   */
  #rows() {
    const rows = new Map();
    for (const h of this.#heard ?? []) {
      rows.set(h.scientificName, { scientificName: h.scientificName, commonName: h.commonName, heard: h, rule: null });
    }
    for (const [name, rule] of [...(this.#baseline?.species ?? []), ...(this.#draft?.species ?? [])]) {
      if (!rows.has(name)) rows.set(name, { scientificName: name, commonName: rule.commonName, heard: null, rule: null });
    }
    for (const [name, rule] of this.#draft?.species ?? []) rows.get(name).rule = rule;
    for (const row of rows.values()) row.change = this.#change(row.scientificName);
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
        .totals { display: flex; flex-wrap: wrap; gap: var(--bs-space-5); margin-bottom: var(--bs-space-6); font-size: 0.875rem; }
        .totals strong { font-family: var(--bs-font-mono); font-weight: 500; font-size: 1.125rem; display: block; }

        .adder { margin-bottom: var(--bs-space-6); border-left: 3px solid var(--bs-accent); }
        .adder h3 { margin-bottom: var(--bs-space-2); }
        .adder .intro { margin-bottom: var(--bs-space-4); }
        .adder .fields { display: flex; flex-wrap: wrap; gap: var(--bs-space-3); align-items: end; }
        .adder .fields > div { flex: 1 1 12rem; }
        .adder .fields > .narrow { flex: 0 1 8rem; }
        .adder .buttons { display: flex; gap: var(--bs-space-2); }
        /* As tall as the fields beside them. */
        .adder .buttons .btn { min-height: 2.25rem; padding-top: 0; padding-bottom: 0; }
        .adder .hint { font-size: 0.8125rem; color: var(--bs-text-body); margin-top: var(--bs-space-3); min-height: 1.3em; }
        .adder .added { margin-top: var(--bs-space-3); padding-top: var(--bs-space-3); border-top: 1px solid var(--bs-callout-border); }
        .adder .added button { margin-left: var(--bs-space-2); }

        .find { display: flex; flex-wrap: wrap; gap: var(--bs-space-3); align-items: end; margin-bottom: var(--bs-space-3); }
        .find .search { flex: 1 1 14rem; }
        .find .filter { min-height: 2.25rem; }
        .field--row { min-height: 2.25rem; font-size: 0.875rem; }
        .name { font-size: 0.9375rem; }
        .latin { font-size: 0.78125rem; font-style: italic; color: var(--bs-text-muted); }
        .rule-note { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.125rem; }
        td { padding-top: 0.625rem; padding-bottom: 0.625rem; }
        td select { min-width: 8.5rem; }
        td input[type="number"] { width: 5.5rem; }
        /* A rule the draft adds, changes or takes away, until it is saved. */
        tr.is-changed { background: var(--bs-callout); box-shadow: inset 3px 0 0 var(--bs-accent); }
        tr.is-changed td:first-child { padding-left: var(--bs-space-3); }
        .change {
          display: inline-block; margin-top: 0.25rem; font-size: 0.6875rem; font-weight: 600; letter-spacing: 0.04em;
          text-transform: uppercase; color: var(--bs-accent-ink);
        }
        .was { font-size: 0.75rem; color: var(--bs-text-muted); }
        .label .change { margin: 0 0 0 var(--bs-space-2); }
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

      ${this.#draft ? this.#editor(rows, totals) : this.#empty()}
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
    const settings = this.#settingsChanged();
    const changes = rows.filter((r) => r.change).length + settings.size;
    const unsaved = !this.#dirty
      ? ""
      : changes
        ? `, with ${count(changes)} ${changes === 1 ? "change" : "changes"} not saved yet.`
        : ", with changes not saved yet.";
    const state = !stored
      ? "Not saved yet."
      : // dateAtTime ends in "a.m." or "p.m.", which is already the full stop.
        `Version ${stored.version}${stored.updatedBy ? `, saved by ${escapeHTML(stored.updatedBy)}` : ""}${stored.updatedAt ? ` on ${escapeHTML(dateAtTime(stored.updatedAt))}` : ""}${unsaved || (stored.updatedAt ? "" : ".")}`;
    const changedLabel = (name) => (settings.has(name) ? `<span class="change">Changed</span>` : "");

    const onlyChanges = this.#onlyChanges && changes > 0;
    const needle = fold(this.#search);
    const shown = rows.filter(
      (r) =>
        (!onlyChanges || r.change) &&
        (!needle || fold(r.commonName).includes(needle) || fold(r.scientificName).includes(needle)),
    );
    const speciesChanges = rows.filter((r) => r.change).length;

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
          <label class="label" for="default-action">Default, for a species with no rule${changedLabel("defaultAction")}</label>
          <select class="field field--sunk" id="default-action" name="defaultAction" data-change="setting">
            ${actionOptions(d.defaults.action)}
          </select>
        </div>
        <div>
          <label class="label" for="default-floor">Its confidence floor${changedLabel("defaultFloor")}</label>
          <input class="field field--sunk" id="default-floor" name="defaultFloor" type="number" min="0" max="1" step="${BAND}"
                 value="${d.defaults.minConfidence}" data-change="setting" />
        </div>
        <div>
          <label class="label" for="bout-gap">Bout gap, minutes${changedLabel("boutGapMinutes")}</label>
          <input class="field field--sunk" id="bout-gap" name="boutGapMinutes" type="number" min="1" max="240" step="1"
                 value="${d.boutGapMinutes}" data-change="setting" />
          <p class="hint">How long a species may go unheard and still be one bout: what a reviewer hears as one, a verdict standing for all of it.</p>
        </div>
        <div>
          <label class="label" for="spot-checks">Spot-checks per species${changedLabel("spotChecksPerSpecies")}</label>
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

      ${this.#adder()}

      <div class="find">
        <div class="search">
          <label class="label" for="rules-search">Find a species in the table</label>
          <input class="field field--sunk field--row" id="rules-search" type="search" value="${escapeHTML(this.#search)}"
                 placeholder="Common or scientific name" data-input="search" />
        </div>
        ${
          speciesChanges
            ? `<button type="button" class="filter" id="only-changes" data-action="onlyChanges" aria-pressed="${onlyChanges}">
                 Only changes (${count(speciesChanges)})
               </button>`
            : ""
        }
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
          <tbody>${shown.map((row, i) => this.#row(row, i)).join("")}</tbody>
        </table>
      </div>
      ${shown.length === 0 ? `<p class="note">No species match.</p>` : ""}
      <p class="note">Counts go by BirdNET's confidence in steps of ${BAND}, so a floor between two steps is counted at the step above it.</p>
    `;
  }

  /** The panel a rule is added or changed in, apart from the table it goes into. */
  #adder() {
    return `
      <section class="panel panel--callout adder" aria-labelledby="adder-title">
        <h3 id="adder-title">Add or change a rule</h3>
        <p class="intro">For a species not in the table below, or to set one's rule here. It goes into the table,
        marked, and takes effect when you save.</p>
        <form id="add-form" data-submit="add">
          <div class="fields">
            <div>
              <label class="label" for="add-scientific">Scientific name</label>
              <input class="field field--sunk field--row" id="add-scientific" name="scientificName" placeholder="Strix varia" required
                     autocomplete="off" list="add-scientific-names" aria-describedby="add-hint" data-input="lookup" />
              <datalist id="add-scientific-names"></datalist>
            </div>
            <div>
              <label class="label" for="add-common">Common name</label>
              <input class="field field--sunk field--row" id="add-common" name="commonName" placeholder="Barred Owl"
                     autocomplete="off" list="add-common-names" aria-describedby="add-hint" data-input="lookup" />
              <datalist id="add-common-names"></datalist>
            </div>
            <div class="narrow">
              <label class="label" for="add-action">Rule</label>
              <select class="field field--sunk field--row" id="add-action" name="action" data-change="touch">
                <option value="review">Review</option>
                <option value="accept">Accept</option>
              </select>
            </div>
            <div class="narrow">
              <label class="label" for="add-floor">Floor</label>
              <input class="field field--sunk field--row" id="add-floor" name="minConfidence" type="number" min="0" max="1"
                     step="${BAND}" value="${this.#draft.defaults.minConfidence}" required data-change="touch" />
            </div>
            <div class="buttons">
              <button class="btn btn--navy btn--small" id="add-confirm" name="confirm">Add to the rules</button>
              <button type="button" class="btn btn--quiet btn--small" data-action="clearAdd">Clear</button>
            </div>
          </div>
          <p class="hint" id="add-hint" aria-live="polite">${escapeHTML(addHint(this.#index, ""))}</p>
        </form>
        ${
          this.#added
            ? `<p class="added" role="status">${escapeHTML(this.#added.text)}
                 <button type="button" class="btn btn--quiet btn--tiny" data-action="showAdded">Show it</button></p>`
            : ""
        }
      </section>`;
  }

  #row(row, i) {
    const { heard, rule, change } = row;
    const d = this.#draft;
    const o = this.#outcome(heard, rule);
    const name = escapeHTML(row.scientificName);
    const reviewed = heard ? heard.confirmed + heard.rejected : 0;
    const dash = `<span class="muted">–</span>`;
    const before = change === "changed" || change === "removed" ? this.#baseline.species.get(row.scientificName) : null;
    return `
      <tr data-species="${name}" ${change ? `class="is-changed"` : ""}>
        <td>
          <div class="name">${escapeHTML(row.commonName)}</div>
          <div class="latin">${name}</div>
          ${rule?.note ? `<div class="rule-note">${escapeHTML(rule.note)}</div>` : ""}
          ${change ? `<div class="change">${CHANGE_LABELS[change]}</div>` : ""}
          ${before ? `<div class="was">Was ${escapeHTML(ruleText(before))}</div>` : ""}
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

/** A rule in words: "reviewed above 0.3". */
function ruleText(rule) {
  return `${rule.action === "accept" ? "accepted" : "reviewed"} above ${rule.minConfidence}`;
}

/** What the add panel says under its fields, given the scientific name typed so far. */
function addHint(names, typed) {
  const general = "Type either name, and BirdNET's list fills in the other.";
  if (!names || !fold(typed)) return general;
  if (names.find(typed, "scientific")) return "";
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

/** A draft's own copy, which editing the draft doesn't touch. */
function copyOf(draft) {
  return draftOf(bodyOf(draft));
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
