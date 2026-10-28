import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { count, dateAtTime, dateRange } from "../format.js";
import * as api from "../api.js";
import { query, replaceQuery } from "../router.js";
import { reviewChip } from "../upload-status.js";
import * as rq from "../review-queue.js";
import * as session from "../session.js";
import { BIRDING_EXPERT } from "../roles.js";
import "./bs-chip.js";
import "./bs-progress-bar.js";
import "./bs-spectrogram.js";

/**
 * <bs-review-card reference="OWL-20261016-SR01"> -- one card's review
 * (REVIEW.md, Screens): its bouts down the side, and the open one to hear and
 * give a verdict on. /app/review/{ref}?bout={id} opens a bout; with none it
 * opens the first that waits for the reviewer.
 *
 * The queue is bouts routed to review, grouped by species, then spot-checks
 * of what the rules accepted; each says where it stands for the reviewer.
 * The open bout plays its most confident detection, with its other best
 * clips a click away and the rest of its detections in a list. Play plays
 * the detection itself; "± 10 s context" plays the whole clip, where the
 * clip was cut with context, and "Half speed" slows it -- both kept from
 * bout to bout, and in this browser.
 *
 * A verdict saves at once and stays changeable until the bout settles; once
 * given, the other listeners' verdicts show (the API hides them until then),
 * and Next goes on to the next bout that waits for the reviewer. Moving
 * between bouts rewrites ?bout= in place, so the page and the clip loaded
 * aren't drawn again, and a reload or a link opens the same bout.
 *
 * A birding expert has the final say as well, on any bout: it settles a
 * disputed one, or overrides how its listeners settled it -- or settles one
 * still open -- for every detection in it. It is open by default only on a
 * disputed bout, and has no keys, so an expert hearing ordinary bouts as a
 * listener can't give it by accident. Opened from the disagreements
 * (?from=disagreements), the way back goes there.
 *
 * Keys, unless focus is in a field: C confirm, R reject, U unsure, space play
 * or pause, ← → the previous and next bout, Enter the next that waits for you
 * once you've given a verdict.
 */

/** Player settings kept between bouts and visits: a per-viewer convenience, so storage may fail. */
const PREFS = "bs-review-player";
function loadPrefs() {
  try {
    return { context: false, slow: false, ...JSON.parse(localStorage.getItem(PREFS) ?? "{}") };
  } catch {
    return { context: false, slow: false };
  }
}
function savePrefs(prefs) {
  try {
    localStorage.setItem(PREFS, JSON.stringify(prefs));
  } catch {
    // Kept for this page only, then.
  }
}

/** How a bout's standing shows as a chip. */
const STANDING_CHIP = { waiting: "neutral", given: "progress", confirmed: "done", rejected: "attention", disputed: "attention" };

class ReviewCard extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];
  static observedAttributes = ["reference"];

  /** {status, card, error}; once ready, #bouts and #queue hold the rest. */
  #state = { status: "loading" };
  /** Every bout on the card, by id, as the API last gave it. */
  #bouts = new Map();
  #queue = { groups: [], order: [] };
  /** The open bout's id. */
  #open = null;
  /** {bout, detections} for each bout loaded, or the promise of it. */
  #details = new Map();
  /** The detection in the player, or null for the bout's most confident. */
  #clip = null;
  #showAll = false;
  /** Where "another species" is open: "verdict", "final" (the final say), or "" for nowhere. */
  #another = "";
  #saving = "";
  #error = "";
  /** Species a confirm can name, once the rules have been read for them. */
  #known = null;
  #prefs = loadPrefs();
  #asked = "";

  connectedCallback() {
    super.connectedCallback();
    document.addEventListener("keydown", this.#onKey);
    this.#load();
  }

  disconnectedCallback() {
    document.removeEventListener("keydown", this.#onKey);
  }

  attributeChangedCallback() {
    if (this.isConnected) this.#load();
  }

  get reference() {
    return this.getAttribute("reference") ?? "";
  }

  get actions() {
    return {
      open: (el) => this.#openBout(el.dataset.bout),
      step: (el) => this.#step(Number(el.dataset.delta)),
      next: () => this.#next(),
      clip: (el) => this.#play(el.dataset.id),
      all: () => {
        this.#showAll = !this.#showAll;
        this.#renderAll();
      },
      verdict: (el) => this.#give(el.dataset.verdict),
      finalSay: (el) => this.#give(el.dataset.verdict, null, true),
      another: (el) => this.#openAnother(el.dataset.for),
      cancelAnother: () => {
        this.#another = "";
        this.#renderVerdict();
      },
      context: (el) => this.#setPref("context", el.checked),
      slow: (el) => this.#setPref("slow", el.checked),
      confirmAs: (form) => this.#confirmAs(form, false),
      finalAs: (form) => this.#confirmAs(form, true),
    };
  }

  async #load() {
    const reference = this.reference;
    if (reference === this.#asked) return;
    this.#asked = reference;
    this.#state = { status: "loading" };
    this.render();
    try {
      const { card, bouts } = await api.fetchBouts(reference);
      if (reference !== this.#asked) return;
      this.#bouts = new Map(bouts.map((b) => [b.id, b]));
      this.#queue = rq.queue(bouts);
      this.#state = { status: "ready", card };
    } catch (error) {
      if (reference !== this.#asked) return;
      this.#state = { status: "error", error };
    }
    if (!this.isConnected) return;
    this.render();
    if (this.#state.status !== "ready") return;
    const asked = query().get("bout");
    const first = this.#bouts.has(asked) ? asked : (rq.nextForMe(this.#queue.order, this.#bouts, null) ?? this.#queue.order[0]);
    if (first) this.#openBout(first, { scroll: true });
  }

  // --- moving between bouts ---

  #openBout(id, { scroll = true } = {}) {
    if (!this.#bouts.has(id)) return;
    const was = this.#open;
    this.#open = id;
    this.#clip = null;
    this.#showAll = false;
    this.#another = "";
    this.#error = "";
    const params = query();
    params.set("bout", id);
    replaceQuery(params);
    for (const at of [was, id]) if (at) this.#renderQueueItem(at);
    if (scroll) this.#reveal(id);
    this.#renderBout();
    // Next is pressed below the player; the next bout starts at its top.
    const top = this.$(".bout")?.getBoundingClientRect().top ?? 0;
    if (was && top < 0) this.$(".bout").scrollIntoView({ block: "start" });
    this.#detail(id).then(() => {
      if (this.#open === id) this.#renderBout();
    });
    // The bout most likely next, so Next is quick.
    const next = rq.nextForMe(this.#queue.order, this.#bouts, id);
    if (next) this.#detail(next).catch(() => {});
  }

  /**
   * Scrolls the queue, and only the queue, to show a bout: scrollIntoView
   * would scroll the page as well, away from the bout just opened.
   */
  #reveal(id) {
    const box = this.$(".queue");
    const item = box?.querySelector(`[data-bout="${CSS.escape(id)}"]`);
    if (!item || box.scrollHeight <= box.clientHeight) return;
    const at = item.getBoundingClientRect();
    const view = box.getBoundingClientRect();
    if (at.top < view.top || at.bottom > view.bottom) box.scrollTop += at.top - view.top - view.height / 3;
  }

  #step(delta) {
    const to = rq.step(this.#queue.order, this.#open, delta);
    if (to) this.#openBout(to);
  }

  #next() {
    const to = rq.nextForMe(this.#queue.order, this.#bouts, this.#open);
    if (to) this.#openBout(to);
  }

  /** A bout and its detections, fetched once; a failure is forgotten, so it is asked again. */
  #detail(id) {
    let got = this.#details.get(id);
    if (!got) {
      got = api.fetchBout(this.reference, id).then(
        (detail) => {
          this.#details.set(id, detail);
          this.#bouts.set(id, detail.bout);
          return detail;
        },
        (error) => {
          this.#details.delete(id);
          throw error;
        },
      );
      this.#details.set(id, got);
    }
    return Promise.resolve(got);
  }

  /** The open bout's {bout, detections}, or null while it loads. */
  get #loaded() {
    const got = this.#details.get(this.#open);
    return got && !(got instanceof Promise) ? got : null;
  }

  // --- verdicts ---

  /** Whether the open bout takes a verdict: open, and not being saved. */
  get #canGive() {
    const b = this.#bouts.get(this.#open);
    return Boolean(b) && b.status === rq.OPEN && !this.#saving;
  }

  /**
   * Gives the reviewer's verdict on the open bout -- or, final, a birding
   * expert's final say -- naming species when it confirms another.
   */
  async #give(verdict, species = null, final = false) {
    const id = this.#open;
    const b = this.#bouts.get(id);
    if (final ? !b || this.#saving : !this.#canGive) return;
    if (!final && b.mine?.verdict === verdict && !species && !b.mine.scientificName) return;
    this.#saving = final ? `final-${verdict}` : verdict;
    this.#error = "";
    this.#renderVerdict();
    try {
      const body = species ? { verdict, ...species } : { verdict };
      const { bout } = await (final ? api.resolveBout : api.giveVerdict)(this.reference, id, body);
      this.#bouts.set(id, bout);
      const detail = this.#loaded && this.#open === id ? this.#loaded : null;
      if (bout.status !== rq.OPEN) {
        // Settled: its detections changed status too, so read them again.
        this.#details.delete(id);
        this.#detail(id).then(() => this.#open === id && this.#renderAll(), () => {});
      } else if (detail) {
        detail.bout = bout;
      }
      this.#another = "";
    } catch (error) {
      this.#error = error.message;
      // Settled or disputed meanwhile: show it as it is now.
      if (error.status === 409) {
        this.#details.delete(id);
        await this.#detail(id).catch(() => {});
      }
    }
    this.#saving = "";
    this.#renderQueueItem(id);
    this.#renderProgress();
    if (this.#open !== id) return;
    this.#renderVerdict();
    this.#renderChip();
    if (!this.#error) this.$(".verdict [data-action=next]")?.focus();
  }

  async #openAnother(where) {
    this.#another = where === "final" ? "final" : "verdict";
    this.#renderVerdict();
    if (!this.#known) {
      const rules = await api.fetchReviewRules().then((r) => r.rules?.species ?? [], () => []);
      this.#known = rq.knownSpecies(rules, [...this.#bouts.values()]);
      this.#renderVerdict();
    }
    this.$(".verdict input[name=species]")?.focus();
  }

  #confirmAs(form, final) {
    const text = new FormData(form).get("species") ?? "";
    const species = rq.parseSpecies(String(text), this.#known ?? []);
    if (!species) {
      this.#error = `Pick a species from the list, or write a new one as "Common name (Scientific name)".`;
      this.#renderVerdict();
      this.$(".verdict input[name=species]")?.focus();
      return;
    }
    this.#give("confirm", species, final);
  }

  // --- the player ---

  #play(id) {
    this.#clip = id;
    this.#renderPlayer();
    this.#renderClipButtons();
    this.$("bs-spectrogram")?.scrollIntoView({ block: "nearest" });
  }

  #setPref(name, on) {
    this.#prefs = { ...this.#prefs, [name]: on };
    savePrefs(this.#prefs);
    const player = this.$("bs-spectrogram");
    if (!player) return;
    if (name === "slow") player.setAttribute("rate", on ? "0.5" : "1");
    else if (on && this.#hasContext()) player.setAttribute("context", "");
    else player.removeAttribute("context");
  }

  /** The detection in the player: the one picked, or the bout's most confident. */
  #playing() {
    const detail = this.#loaded;
    if (!detail) return null;
    const id = this.#clip ?? detail.bout.best[0];
    return detail.detections.find((d) => d.id === id) ?? detail.detections[0] ?? null;
  }

  /** Whether the clip in the player was cut with more than a second or so either side. */
  #hasContext() {
    const d = this.#playing();
    return Boolean(d?.clip) && (d.startSec - d.clip.startSec > 2 || d.clip.endSec - d.endSec > 2);
  }

  #onKey = (event) => {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
    if (this.#state.status !== "ready" || !this.#open) return;
    const focus = event.composedPath()[0];
    if (focus instanceof Element && focus.closest("input, textarea, select, [contenteditable]")) return;
    const onControl = focus instanceof Element && focus.closest("button, a, summary");
    const key = event.key.length === 1 ? event.key.toLowerCase() : event.key;
    const verdict = { c: "confirm", r: "reject", u: "unsure" }[key];
    if (verdict && !event.shiftKey) {
      event.preventDefault();
      this.#give(verdict);
    } else if (key === " " && !onControl) {
      event.preventDefault();
      if (!event.repeat) this.$("bs-spectrogram")?.toggle();
    } else if (key === "ArrowLeft" || key === "ArrowRight") {
      event.preventDefault();
      this.#step(key === "ArrowLeft" ? -1 : 1);
    } else if (key === "Enter" && !onControl && !this.#bouts.get(this.#open)?.forMe) {
      event.preventDefault();
      this.#next();
    }
  };

  // --- drawing ---

  render() {
    const { status, error, card } = this.#state;
    this.shadowRoot.innerHTML = `
      <style>
        .back { display: inline-block; font-size: 0.875rem; margin-bottom: var(--bs-space-4); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; color: var(--bs-text-muted); }
        .card-sub { margin: var(--bs-space-2) 0 var(--bs-space-4); font-size: 0.90625rem; color: var(--bs-text-body); }
        .progress { max-width: 36rem; margin-bottom: var(--bs-space-6); }
        .progress p { margin-top: var(--bs-space-2); font-size: 0.8125rem; color: var(--bs-text-muted); }

        .layout {
          display: grid;
          grid-template-columns: minmax(14rem, 18rem) minmax(0, 1fr);
          grid-template-areas: "queue bout";
          gap: var(--bs-space-6);
          align-items: start;
        }
        .bout { grid-area: bout; min-width: 0; }
        .queue {
          grid-area: queue;
          position: sticky;
          top: var(--bs-space-4);
          max-height: calc(100vh - 2 * var(--bs-space-4));
          overflow-y: auto;
          border: 1px solid var(--bs-border);
          background: var(--bs-surface);
          padding: var(--bs-space-3) 0;
        }
        @media (max-width: 820px) {
          .layout { grid-template-columns: minmax(0, 1fr); grid-template-areas: "bout" "queue"; }
          .queue { position: static; max-height: 24rem; }
        }
        .queue h3 {
          font-family: var(--bs-font-mono);
          font-size: 0.65625rem;
          letter-spacing: 0.14em;
          text-transform: uppercase;
          color: var(--bs-text-muted);
          margin: var(--bs-space-3) var(--bs-space-4) var(--bs-space-1);
        }
        .queue h4 {
          display: flex;
          justify-content: space-between;
          gap: var(--bs-space-2);
          font-size: 0.8125rem;
          font-weight: 600;
          margin: var(--bs-space-3) var(--bs-space-4) var(--bs-space-1);
          color: var(--bs-text);
        }
        .queue h4 span { font-weight: 400; color: var(--bs-text-muted); white-space: nowrap; }
        .queue ol { list-style: none; margin: 0; padding: 0; }
        .item {
          display: flex;
          align-items: center;
          gap: var(--bs-space-2);
          padding: 0.3125rem var(--bs-space-4);
          font-size: 0.8125rem;
          color: var(--bs-text-soft);
          text-decoration: none;
          border-left: 3px solid transparent;
        }
        .item:hover { background: var(--bs-bg); color: var(--bs-text); }
        .item[aria-current="true"] { border-left-color: var(--bs-accent); background: var(--bs-bg); color: var(--bs-text); font-weight: 600; }
        .item .n { margin-left: auto; color: var(--bs-text-muted); font-weight: 400; }
        .dot { flex: none; width: 0.625rem; height: 0.625rem; border-radius: 50%; border: 1.5px solid var(--bs-border-strong); }
        .mark-waiting .dot { border-color: var(--bs-accent); background: var(--bs-accent); }
        .mark-given .dot { border-color: var(--bs-accent); }
        .mark-confirmed .dot { border-color: var(--bs-chip-done-border); background: var(--bs-chip-done-bg); }
        .mark-rejected .dot, .mark-disputed .dot { border-color: var(--bs-chip-attention-border); background: var(--bs-chip-attention-bg); }
        .mark-confirmed, .mark-rejected { color: var(--bs-text-muted); }

        .nav { display: flex; align-items: baseline; justify-content: space-between; gap: var(--bs-space-4); flex-wrap: wrap; margin-bottom: var(--bs-space-3); }
        .steps { display: flex; gap: var(--bs-space-2); }
        .head { display: flex; align-items: baseline; gap: var(--bs-space-3) var(--bs-space-4); flex-wrap: wrap; }
        .head h2 { margin: 0; }
        .sci { font-family: var(--bs-font-display); font-style: italic; font-size: 1.0625rem; color: var(--bs-text-muted); }
        .sub { margin-top: var(--bs-space-2); font-size: 0.90625rem; color: var(--bs-text-body); }
        .kind-note { margin-top: var(--bs-space-3); }

        .clips { display: flex; gap: var(--bs-space-2); flex-wrap: wrap; align-items: center; margin: var(--bs-space-5) 0 var(--bs-space-3); }
        .clips .btn[aria-pressed="true"] { box-shadow: inset 0 0 0 2px currentColor; }
        .player { margin-bottom: var(--bs-space-3); }
        .options { display: flex; gap: var(--bs-space-5); flex-wrap: wrap; font-size: 0.875rem; color: var(--bs-text-body); margin-bottom: var(--bs-space-5); }
        .options label { display: inline-flex; align-items: center; gap: var(--bs-space-2); }
        .options label.off { color: var(--bs-text-muted); }
        .quiet { color: var(--bs-text-muted); font-size: 0.875rem; }

        .verdict .question { margin-bottom: var(--bs-space-4); font-size: 0.9375rem; color: var(--bs-text); }
        .verdict .btn[aria-pressed="true"] { box-shadow: inset 0 0 0 2px currentColor; }
        .verdict kbd {
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
          opacity: 0.75;
          margin-left: var(--bs-space-1);
        }
        .verdict .another { margin-top: var(--bs-space-4); display: flex; gap: var(--bs-space-2); flex-wrap: wrap; align-items: end; }
        .verdict .another label { flex: 1 1 18rem; }
        .verdict .error { margin-top: var(--bs-space-3); }
        .verdict .listeners { margin: var(--bs-space-4) 0 0; padding: 0; list-style: none; font-size: 0.875rem; }
        .verdict .listeners li { padding: var(--bs-space-1) 0; color: var(--bs-text-body); }
        .verdict .listeners .you { font-weight: 600; }
        .verdict .final { margin-top: var(--bs-space-5); padding-top: var(--bs-space-4); border-top: 1px solid var(--bs-border); }
        .verdict .final summary { cursor: pointer; font-weight: 600; color: var(--bs-text); margin-bottom: var(--bs-space-2); }
        .verdict .final .row { margin-top: var(--bs-space-3); }
        .verdict .then { margin-top: var(--bs-space-4); display: flex; gap: var(--bs-space-4); align-items: center; flex-wrap: wrap; }

        .all { margin-top: var(--bs-space-6); }
        .all td.num { white-space: nowrap; }
      </style>
      ${
        status === "loading"
          ? `<a class="back" href="/app/review">← All cards</a><p class="empty">Loading the card's bouts…</p>`
          : status === "error"
            ? `<a class="back" href="/app/review">← All cards</a><p class="empty">Couldn't load this card's bouts: ${escapeHTML(error.message)}</p>`
            : this.#page(card)
      }
    `;
  }

  #page(card) {
    return `
      ${this.#back()}
      <h1>${escapeHTML(card.stationName || card.stationId)}</h1>
      <p class="card-sub">${escapeHTML(dateRange(card.firstRecordingAt, card.lastRecordingAt))} · <span class="ref">${escapeHTML(card.reference)}</span></p>
      <div class="progress">${this.#progress()}</div>
      ${
        this.#bouts.size === 0
          ? `<p class="empty">This card has no bouts to review.</p>`
          : `<div class="layout">
               <section class="bout" aria-live="polite"></section>
               <nav class="queue" aria-label="Bouts on this card">${this.#queueList()}</nav>
             </div>`
      }
    `;
  }

  /** The way back: to the disagreements, when the bout was opened from them, or to every card. */
  #back() {
    return query().get("from") === "disagreements"
      ? `<a class="back" href="/app/disagreements">← Disagreements</a>`
      : `<a class="back" href="/app/review">← All cards</a>`;
  }

  #progress() {
    const p = rq.progress([...this.#bouts.values()]);
    return `
      <bs-progress-bar size="thin" value="${p.total ? (p.settled / p.total) * 100 : 0}"
        label="${count(p.settled)} of ${count(p.total)} bouts settled"></bs-progress-bar>
      <p>${count(p.settled)} of ${count(p.total)} bouts settled · ${
        p.left ? `${count(p.left)} waiting for you` : "nothing waiting for you"
      }${p.disputed ? ` · ${count(p.disputed)} disputed` : ""}</p>
    `;
  }

  #queueList() {
    return this.#queue.groups
      .map(
        (g) => `
      <h3>${g.kind === rq.SPOT_CHECK ? "Spot-checks" : "To review"}</h3>
      ${g.species
        .map((s) => {
          const left = s.bouts.filter((b) => this.#bouts.get(b.id)?.forMe).length;
          return `
        <h4>${escapeHTML(s.commonName)} <span>${left ? `${count(left)} of ${count(s.bouts.length)} left` : count(s.bouts.length)}</span></h4>
        <ol>${s.bouts.map((b) => `<li>${this.#queueItem(b.id)}</li>`).join("")}</ol>`;
        })
        .join("")}`,
      )
      .join("");
  }

  #queueItem(id) {
    const b = this.#bouts.get(id);
    const { mark, label } = rq.standing(b);
    const href = `/app/review/${encodeURIComponent(this.reference)}?bout=${encodeURIComponent(id)}`;
    return `
      <a class="item mark-${mark}" href="${escapeHTML(href)}" data-action="open" data-bout="${escapeHTML(id)}"
         ${id === this.#open ? 'aria-current="true"' : ""}>
        <span class="dot" aria-hidden="true"></span>
        <span>${escapeHTML(dateAtTime(b.firstHeardAt))}</span>
        ${b.detections > 1 ? `<span class="n">×${count(b.detections)}</span>` : ""}
        <span class="visually-hidden">, ${escapeHTML(label)}</span>
      </a>`;
  }

  #renderQueueItem(id) {
    const link = this.$(`.queue [data-bout="${CSS.escape(id)}"]`);
    if (!link) return;
    link.outerHTML = this.#queueItem(id);
    // The species' "left" count may have moved too.
    const list = this.$(`.queue [data-bout="${CSS.escape(id)}"]`)?.closest("ol");
    const head = list?.previousElementSibling;
    if (head?.tagName === "H4") {
      const ids = [...list.querySelectorAll("[data-bout]")].map((a) => a.dataset.bout);
      const left = ids.filter((b) => this.#bouts.get(b)?.forMe).length;
      head.querySelector("span").textContent = left ? `${count(left)} of ${count(ids.length)} left` : count(ids.length);
    }
  }

  #renderProgress() {
    const el = this.$(".progress");
    if (el) el.innerHTML = this.#progress();
  }

  /** The open bout, whole: drawn when it opens, and when its detections arrive. */
  #renderBout() {
    const el = this.$(".bout");
    if (!el) return;
    const b = this.#bouts.get(this.#open);
    if (!b) return void (el.innerHTML = "");
    const order = this.#queue.order;
    const at = order.indexOf(b.id);
    const ofKind = order.filter((id) => this.#bouts.get(id).kind === b.kind);
    const prev = rq.step(order, b.id, -1);
    const next = rq.step(order, b.id, 1);
    const detail = this.#loaded;

    el.innerHTML = `
      <div class="nav">
        <span class="eyebrow">${b.kind === rq.SPOT_CHECK ? "Spot-check" : "Bout"} ${count(ofKind.indexOf(b.id) + 1)} of ${count(ofKind.length)}</span>
        <span class="steps">
          <button class="btn btn--quiet btn--small" data-action="step" data-delta="-1" ${prev ? "" : "disabled"}>← Previous</button>
          <button class="btn btn--quiet btn--small" data-action="step" data-delta="1" ${next ? "" : "disabled"}>Next →</button>
        </span>
      </div>
      <div class="head">
        <h2>${escapeHTML(b.commonName)}</h2>
        ${b.scientificName !== b.commonName ? `<span class="sci">${escapeHTML(b.scientificName)}</span>` : ""}
        <span class="chip-slot">${this.#chip(b)}</span>
      </div>
      <p class="sub">
        ${escapeHTML(dateAtTime(b.firstHeardAt))}${b.lastHeardAt !== b.firstHeardAt ? ` to ${escapeHTML(dateAtTime(b.lastHeardAt))}` : ""} ·
        ${b.detections === 1 ? "1 detection" : `${count(b.detections)} detections`} · best ${Math.round(b.maxConfidence * 100)}%
      </p>
      ${
        b.kind === rq.SPOT_CHECK
          ? `<p class="note kind-note">A spot-check: the review rules accept ${escapeHTML(b.commonName)} without anyone listening, so this is
             a sample of what they accepted. Is BirdNET right about it here?</p>`
          : ""
      }
      ${
        detail
          ? `<div class="clips" role="group" aria-label="Its most confident clips">${this.#clipButtons(detail)}</div>
             <div class="player"></div>
             <div class="options">${this.#options()}</div>`
          : `<p class="quiet clips">Loading the bout's clips…</p>`
      }
      <section class="panel verdict" aria-live="polite"></section>
      <div class="all"></div>
    `;
    this.#renderVerdict();
    this.#renderPlayer();
    this.#renderAll();
  }

  #chip(b) {
    const { mark, label } = rq.standing(b);
    return `<bs-chip kind="${STANDING_CHIP[mark]}">${escapeHTML(label)}</bs-chip>`;
  }

  #renderChip() {
    const slot = this.$(".chip-slot");
    const b = this.#bouts.get(this.#open);
    if (slot && b) slot.innerHTML = this.#chip(b);
  }

  #clipButtons(detail) {
    const playing = this.#playing();
    return detail.bout.best
      .map((id, i) => {
        const d = detail.detections.find((x) => x.id === id);
        if (!d) return "";
        return `<button class="btn btn--quiet btn--small" data-action="clip" data-id="${escapeHTML(id)}"
                        aria-pressed="${playing?.id === id}">Clip ${i + 1} · ${Math.round(d.confidence * 100)}%</button>`;
      })
      .join("");
  }

  #renderClipButtons() {
    const el = this.$(".clips");
    if (el && this.#loaded) el.innerHTML = this.#clipButtons(this.#loaded);
    this.#renderAll();
    this.#renderOptions();
  }

  #options() {
    const context = this.#hasContext();
    return `
      <label class="${context ? "" : "off"}" title="${context ? "" : "This clip was cut without context either side"}">
        <input type="checkbox" data-change="context" ${this.#prefs.context && context ? "checked" : ""} ${context ? "" : "disabled"}>
        ± 10 s context
      </label>
      <label><input type="checkbox" data-change="slow" ${this.#prefs.slow ? "checked" : ""}> Half speed</label>
    `;
  }

  #renderOptions() {
    const el = this.$(".options");
    if (el) el.innerHTML = this.#options();
  }

  /** The spectrogram, for the detection in the player. */
  #renderPlayer() {
    const el = this.$(".player");
    const detail = this.#loaded;
    const d = this.#playing();
    if (!el || !detail) return;
    if (!d?.clip) {
      el.innerHTML = `<p class="quiet">There's no clip of this detection: its file was analyzed before clips were cut.</p>`;
      return;
    }
    el.innerHTML = `
      <bs-spectrogram src="${escapeHTML(api.clipURL(this.reference, d.id))}" clip-start="${d.clip.startSec}"
        focus-start="${d.startSec}" focus-end="${d.endSec}" rate="${this.#prefs.slow ? "0.5" : "1"}"
        ${this.#prefs.context && this.#hasContext() ? "context" : ""}></bs-spectrogram>`;
    // The bout's detections in the same file are what the clip may hold of it.
    const span = (x) => ({ startSec: x.startSec, endSec: x.endSec });
    el.querySelector("bs-spectrogram").marks = [
      { label: d.commonName, spans: detail.detections.filter((x) => x.audioFileId === d.audioFileId).map(span) },
    ];
  }

  /** The bout's every detection, a click away. */
  #renderAll() {
    const el = this.$(".all");
    const detail = this.#loaded;
    if (!el || !detail) return;
    const n = detail.detections.length;
    if (n <= 1) return void (el.innerHTML = "");
    const playing = this.#playing();
    el.innerHTML = `
      <button class="btn btn--quiet btn--small" data-action="all" aria-expanded="${this.#showAll}">
        ${this.#showAll ? "Hide" : "Show"} all ${count(n)} detections in this bout
      </button>
      ${
        this.#showAll
          ? `<div class="table-scroll">
               <table>
                 <thead><tr><th scope="col">Heard</th><th scope="col" style="text-align: right;">Confidence</th><th scope="col">Status</th><th scope="col"><span class="visually-hidden">Play</span></th></tr></thead>
                 <tbody>
                   ${detail.detections
                     .map((d) => {
                       const chip = reviewChip(d.reviewStatus);
                       return `<tr>
                     <td>${escapeHTML(dateAtTime(d.detectedAt))}</td>
                     <td class="num">${Math.round(d.confidence * 100)}%</td>
                     <td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>
                     <td>${
                       d.clip
                         ? `<button class="btn btn--quiet btn--small" data-action="clip" data-id="${escapeHTML(d.id)}"
                                    aria-pressed="${playing?.id === d.id}">${playing?.id === d.id ? "In the player" : "Play"}</button>`
                         : `<span class="quiet">No clip</span>`
                     }</td>
                   </tr>`;
                     })
                     .join("")}
                 </tbody>
               </table>
             </div>`
          : ""
      }
    `;
  }

  /** The verdict panel: what to say, what was said, and what's next. */
  #renderVerdict() {
    const el = this.$(".verdict");
    const b = this.#bouts.get(this.#open);
    if (!el || !b) return;
    const article = /^[aeiou]/i.test(b.commonName) ? "an" : "a";
    const open = b.status === rq.OPEN;
    const busy = !this.#canGive ? "disabled" : "";
    const pressed = (v) => b.mine?.verdict === v && !(v === "confirm" && b.mine.scientificName);
    const label = (v, idle, doing) => (this.#saving === v ? doing : idle);
    const next = rq.nextForMe(this.#queue.order, this.#bouts, b.id);

    const finalBy = b.resolution
      ? `Settled by ${escapeHTML(b.resolution.by)}'s final say: <strong>${escapeHTML(rq.verdictLabel(b.resolution, b))}</strong>, for every detection in it.`
      : "";
    const question = finalBy || {
      [rq.OPEN]: b.mine
        ? `Your verdict: <strong>${escapeHTML(rq.verdictLabel(b.mine, b))}</strong>. You can change it until the bout is settled.`
        : `Listen to the clips. Is this ${article} ${escapeHTML(b.commonName)}?`,
      confirmed: "Settled: its listeners agreed, and their verdict counts for every detection in it.",
      rejected: "Settled: its listeners agreed it isn't this bird, so none of its detections count.",
      [rq.DISPUTED]: session.has(BIRDING_EXPERT)
        ? "Its three listeners didn't agree, so it waits for your final say, below."
        : "Its three listeners didn't agree, so it waits for a birding expert.",
    }[b.status];

    el.innerHTML = `
      <h3>Your verdict</h3>
      <p class="question">${question ?? ""}</p>
      ${
        open
          ? `<div class="row">
               <button class="btn btn--navy btn--small" data-action="verdict" data-verdict="confirm" aria-pressed="${pressed("confirm")}" ${busy}>
                 ${label("confirm", "Confirm", "Saving…")}<kbd aria-hidden="true">C</kbd></button>
               <button class="btn btn--quiet btn--danger btn--small" data-action="verdict" data-verdict="reject" aria-pressed="${pressed("reject")}" ${busy}>
                 ${label("reject", "Reject", "Saving…")}<kbd aria-hidden="true">R</kbd></button>
               <button class="btn btn--quiet btn--small" data-action="verdict" data-verdict="unsure" aria-pressed="${pressed("unsure")}" ${busy}>
                 ${label("unsure", "Unsure", "Saving…")}<kbd aria-hidden="true">U</kbd></button>
               ${
                 this.#another === "verdict"
                   ? ""
                   : `<button class="btn btn--quiet btn--small" data-action="another" data-for="verdict" aria-pressed="${Boolean(b.mine?.scientificName)}" ${busy}>Another species…</button>`
               }
             </div>
             ${this.#another === "verdict" ? this.#anotherForm(false) : ""}`
          : ""
      }
      ${this.#error ? `<p class="error" role="alert">Couldn't save that: ${escapeHTML(this.#error)}</p>` : ""}
      ${this.#listeners(b)}
      ${session.has(BIRDING_EXPERT) ? this.#finalSay(b) : ""}
      ${
        b.forMe
          ? ""
          : `<div class="then">${
              next
                ? `<button class="btn btn--navy btn--small" data-action="next">Next bout for you →</button>
                   <span class="note">or press Enter</span>`
                : `<p class="note">That's every bout waiting for you on this card.</p>`
            }</div>`
      }
    `;
  }

  /**
   * A birding expert's final say: open by default on a disputed bout, and a
   * click away on any other, where it overrides its listeners.
   */
  #finalSay(b) {
    const busy = this.#saving ? "disabled" : "";
    const label = (v, idle) => (this.#saving === `final-${v}` ? "Saving…" : idle);
    const lead =
      b.status === rq.DISPUTED
        ? "Your final say settles it, for every detection in it."
        : b.resolution
          ? "Another final say replaces this one."
          : b.status === rq.OPEN
            ? "A final say settles it now, without waiting for its listeners."
            : "Its listeners settled it. A final say overrides them, for every detection in it.";
    const open = b.status === rq.DISPUTED || this.#another === "final" || this.#saving.startsWith("final-");
    return `
      <details class="final" ${open ? "open" : ""}>
        <summary>Final say</summary>
        <p class="note">${lead}</p>
        <div class="row">
          <button class="btn btn--navy btn--small" data-action="finalSay" data-verdict="confirm" ${busy}>${label("confirm", "Confirm")}</button>
          <button class="btn btn--quiet btn--danger btn--small" data-action="finalSay" data-verdict="reject" ${busy}>${label("reject", "Reject")}</button>
          ${
            this.#another === "final"
              ? ""
              : `<button class="btn btn--quiet btn--small" data-action="another" data-for="final" ${busy}>Another species…</button>`
          }
        </div>
        ${this.#another === "final" ? this.#anotherForm(true) : ""}
      </details>`;
  }

  #anotherForm(final) {
    const known = this.#known;
    return `
      <form class="another" data-submit="${final ? "finalAs" : "confirmAs"}">
        <label>
          <span class="label">Which species is it?</span>
          <input class="field field--sunk" name="species" list="species-options" autocomplete="off"
                 placeholder="${known ? "Common name, or Common name (Scientific name)" : "Loading species…"}"
                 value="${escapeHTML(this.#bouts.get(this.#open)?.mine?.scientificName ? rq.speciesOption(this.#bouts.get(this.#open).mine) : "")}">
        </label>
        <datalist id="species-options">
          ${(known ?? []).map((s) => `<option value="${escapeHTML(rq.speciesOption(s))}"></option>`).join("")}
        </datalist>
        <button class="btn btn--navy btn--small" type="submit" ${this.#saving ? "disabled" : ""}>${final ? "Final say: this species" : "Confirm as this species"}</button>
        <button class="btn btn--quiet btn--small" type="button" data-action="cancelAnother">Cancel</button>
      </form>`;
  }

  /** Everyone's verdicts, once the reviewer may see them. */
  #listeners(b) {
    if (!b.verdicts) {
      return b.listeners
        ? `<p class="note">${b.listeners === 1 ? "One listener has" : `${count(b.listeners)} listeners have`} given a verdict. You'll see theirs once you give yours.</p>`
        : "";
    }
    const me = b.mine;
    return `
      <ul class="listeners" aria-label="Verdicts given">
        ${b.verdicts
          .map((v) => {
            const yours = me && v.by === me.by && v.at === me.at;
            return `<li class="${yours ? "you" : ""}">${escapeHTML(v.by)}${yours ? " (you)" : ""}: ${escapeHTML(rq.verdictLabel(v, b))}</li>`;
          })
          .join("")}
      </ul>
      ${
        b.status === rq.OPEN && b.verdicts.length > 0
          ? `<p class="note">${b.kind === rq.SPOT_CHECK ? "A spot-check settles on one confirm; anything else needs two who agree." : "Two who agree settle it."}</p>`
          : ""
      }`;
  }
}

customElements.define("bs-review-card", ReviewCard);
