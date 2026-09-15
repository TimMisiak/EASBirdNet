import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, panels, typography } from "../shared-styles.js";
import { clock, dateAtTime, longDate, shortDate } from "../format.js";
import { reviewChip } from "../upload-status.js";
import * as api from "../api.js";
import { navigate } from "../router.js";
import "./bs-chip.js";
import "./bs-spectrogram.js";

/**
 * <bs-detection-detail reference="OWL-20260914-SR03" detection="det_…"> -- one
 * detection, for review: its clip to hear and see, and Confirm or Discard.
 * Previous and next step through the other detections in the same file.
 *
 * It is opened from a card's page, and goes back there, or from the list of
 * every detection, which it goes back to with that list's filters. Its links
 * stay under whichever it was opened from.
 *
 * The page is rendered whole once the detection loads. A verdict redraws only
 * the review panel and the chip, so a clip that is playing keeps playing.
 *
 * Keys: ← and → go to the previous and next detection, and space plays or
 * pauses the clip, unless focus is somewhere those keys already mean something.
 *
 * Attributes: reference, the card; detection, the detection's id; list, present
 * when it was opened from the list of every detection, holding that list's
 * query string.
 */

class DetectionDetail extends BaseElement {
  static styles = [typography, controls, panels];
  static observedAttributes = ["reference", "detection", "list"];

  /** {status, upload, file, detection, siblings, error}; siblings are the file's detections, in order. */
  #state = { status: "loading" };
  /** The verdict being saved, if one is. */
  #saving = "";
  #saveError = "";

  connectedCallback() {
    super.connectedCallback();
    document.addEventListener("keydown", this.#onKey);
    this.#load();
  }

  disconnectedCallback() {
    document.removeEventListener("keydown", this.#onKey);
  }

  /** ← and → step through the file's detections; space plays or pauses the clip. */
  #onKey = (event) => {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey || event.shiftKey) return;
    if (this.#state.status !== "ready") return;
    // Focus is in a shadow root, so look along the composed path, not at event.target.
    const focus = event.composedPath()[0];
    const typing = focus instanceof Element && focus.closest("input, textarea, select, [contenteditable]");
    if (typing) return;

    if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      const { prev, next } = this.#neighbours();
      const to = event.key === "ArrowLeft" ? prev : next;
      if (!to) return;
      event.preventDefault();
      navigate(this.#href(to.id));
    } else if (event.key === " ") {
      // A focused button or link already answers space itself.
      if (focus instanceof Element && focus.closest("button, a")) return;
      const player = this.$("bs-spectrogram");
      if (!player) return;
      event.preventDefault();
      if (!event.repeat) player.toggle();
    }
  };

  /** The detections either side of this one in its file, or null at either end. */
  #neighbours() {
    const { detection, siblings } = this.#state;
    const at = siblings.findIndex((s) => s.id === detection.id);
    return {
      at,
      prev: at > 0 ? siblings[at - 1] : null,
      next: at >= 0 && at < siblings.length - 1 ? siblings[at + 1] : null,
    };
  }

  attributeChangedCallback() {
    if (!this.isConnected) return;
    this.#state = { status: "loading" };
    this.#saving = this.#saveError = "";
    this.render();
    this.#load();
  }

  get reference() {
    return this.getAttribute("reference") ?? "";
  }

  get detectionId() {
    return this.getAttribute("detection") ?? "";
  }

  get actions() {
    return {
      review: (el) => this.#review(el.dataset.status),
    };
  }

  async #load() {
    const reference = this.reference;
    const id = this.detectionId;
    let next;
    try {
      const { upload, file, detection } = await api.fetchDetection(reference, id);
      // Previous and next are a convenience; the page works without them.
      const siblings = await api
        .fetchFileDetections(reference, file.id)
        .then((body) => body.detections)
        .catch(() => []);
      next = { status: "ready", upload, file, detection, siblings };
    } catch (error) {
      next = { status: "error", error };
    }
    if (reference !== this.reference || id !== this.detectionId) return;
    this.#state = next;
    if (this.isConnected) this.render();
  }

  async #review(status) {
    const { detection, siblings } = this.#state;
    if (!detection || this.#saving || status === detection.reviewStatus) return;
    this.#saving = status;
    this.#saveError = "";
    this.#renderReview();
    try {
      const { detection: saved } = await api.reviewDetection(this.reference, detection.id, status);
      if (saved.id !== this.#state.detection?.id) return;
      this.#state.detection = saved;
      const i = siblings.findIndex((d) => d.id === saved.id);
      if (i >= 0) siblings[i] = saved;
    } catch (error) {
      this.#saveError = error.message;
    }
    this.#saving = "";
    this.#renderReview();
  }

  /** The list's query string, with its "?", or "" -- when opened from the list of every detection. */
  get #listSearch() {
    const search = this.getAttribute("list") ?? "";
    return search ? `?${search}` : "";
  }

  #href(id) {
    const ref = encodeURIComponent(this.reference);
    return this.hasAttribute("list")
      ? `/admin/detections/${ref}/${encodeURIComponent(id)}${this.#listSearch}`
      : `/admin/uploads/${ref}/detections/${encodeURIComponent(id)}`;
  }

  render() {
    const { status, error } = this.#state;
    const back = this.hasAttribute("list")
      ? { href: `/admin/detections${this.#listSearch}`, label: "All detections" }
      : { href: `/admin/uploads/${encodeURIComponent(this.reference)}`, label: this.reference };

    this.shadowRoot.innerHTML = `
      <style>
        .back { display: inline-block; font-size: 0.875rem; margin-bottom: var(--bs-space-4); }
        .nav {
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: var(--bs-space-4);
          flex-wrap: wrap;
          margin-bottom: var(--bs-space-3);
        }
        .steps { display: flex; gap: var(--bs-space-4); font-size: 0.875rem; }
        .steps .off { color: var(--bs-text-muted); }
        .head { display: flex; align-items: baseline; gap: var(--bs-space-3) var(--bs-space-4); flex-wrap: wrap; }
        .sci { font-family: var(--bs-font-display); font-style: italic; font-size: 1.125rem; color: var(--bs-text-muted); }
        .sub { margin-top: var(--bs-space-2); font-size: 0.90625rem; color: var(--bs-text-body); }

        .clip { margin: var(--bs-space-6) 0; }
        .quiet { color: var(--bs-text-muted); font-size: 0.875rem; }

        .layout { display: grid; grid-template-columns: minmax(0, 1fr) minmax(16rem, 22rem); gap: var(--bs-space-6); align-items: start; }
        @media (max-width: 760px) { .layout { grid-template-columns: minmax(0, 1fr); } }

        .facts { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: var(--bs-space-3) var(--bs-space-5); margin: 0; font-size: 0.875rem; }
        .facts dt { font-family: var(--bs-font-mono); font-size: 0.65625rem; letter-spacing: 0.14em; text-transform: uppercase; color: var(--bs-text-muted); padding-top: 0.1875rem; }
        .facts dd { margin: 0; color: var(--bs-text-soft); overflow-wrap: anywhere; }
        .facts small { display: block; color: var(--bs-text-muted); font-size: 0.78125rem; }
        .path { font-family: var(--bs-font-mono); font-size: 0.8125rem; }

        .review h3 { margin-bottom: var(--bs-space-2); }
        .review .verdict { margin-bottom: var(--bs-space-4); }
        .review .row { gap: var(--bs-space-2); }
        .review .btn[aria-pressed="true"] { box-shadow: inset 0 0 0 2px currentColor; }
        .review .error { margin-top: var(--bs-space-3); }
        .review .next { display: inline-block; margin-top: var(--bs-space-4); font-size: 0.875rem; }
        .review .note { margin-top: var(--bs-space-4); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
      </style>

      <a class="back" href="${escapeHTML(back.href)}">← ${escapeHTML(back.label)}</a>
      ${
        status === "loading"
          ? `<p class="empty">Loading the detection…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load this detection: ${escapeHTML(error.message)}</p>`
            : this.#page()
      }
    `;
  }

  #page() {
    const { upload, file, detection: d, siblings } = this.#state;
    const { at, prev, next } = this.#neighbours();
    const span = d.endSec - d.startSec;
    const windows = Math.round(span / 3);

    return `
      <div class="nav">
        <span class="eyebrow">${at >= 0 ? `Detection ${at + 1} of ${siblings.length} in this file` : "Detection"}</span>
        ${
          siblings.length > 1
            ? `<span class="steps">
                 ${prev ? `<a href="${escapeHTML(this.#href(prev.id))}">← Previous</a>` : `<span class="off">← Previous</span>`}
                 ${next ? `<a href="${escapeHTML(this.#href(next.id))}">Next →</a>` : `<span class="off">Next →</span>`}
               </span>`
            : ""
        }
      </div>
      <div class="head">
        <h1>${escapeHTML(d.commonName)}</h1>
        ${d.scientificName && d.scientificName !== d.commonName ? `<span class="sci">${escapeHTML(d.scientificName)}</span>` : ""}
        <span class="chip-slot">${this.#chip()}</span>
      </div>
      <p class="sub">
        ${escapeHTML(upload.stationName)} · ${escapeHTML(dateAtTime(d.detectedAt))} ·
        ${Math.round(d.confidence * 100)}% confidence
      </p>

      <div class="clip">
        ${
          d.clip
            ? `<bs-spectrogram
                 src="${escapeHTML(api.clipURL(this.reference, d.id))}"
                 clip-start="${d.clip.startSec}"
                 highlight-start="${d.startSec}"
                 highlight-end="${d.endSec}"></bs-spectrogram>`
            : `<p class="quiet">There's no clip of this detection: its file was analyzed before clips were cut.</p>`
        }
      </div>

      <div class="layout">
        <dl class="facts">
          <dt>Heard</dt>
          <dd>
            ${clock(d.startSec)}–${clock(d.endSec)} into the recording
            <small>${windows > 1 ? `${windows} consecutive 3-second windows, merged` : "One 3-second window"}</small>
          </dd>
          <dt>Confidence</dt>
          <dd>
            ${Math.round(d.confidence * 100)}%
            ${windows > 1 ? `<small>The most confident of its windows</small>` : ""}
          </dd>
          <dt>File</dt>
          <dd><span class="path">${escapeHTML(file.path).replaceAll("/", "/<wbr>")}</span></dd>
          <dt>Night</dt>
          <dd>${escapeHTML(shortDate(file.night))}</dd>
          <dt>Card</dt>
          <dd>
            <a class="path" href="/admin/uploads/${encodeURIComponent(upload.reference)}">${escapeHTML(upload.reference)}</a>
            <small>${escapeHTML(upload.volunteerName)}, pulled ${escapeHTML(longDate(upload.pulledOn))}</small>
          </dd>
        </dl>
        <section class="panel review" aria-live="polite">${this.#reviewPanel(next)}</section>
      </div>
    `;
  }

  #chip() {
    const { kind, label } = reviewChip(this.#state.detection.reviewStatus);
    return `<bs-chip kind="${kind}">${escapeHTML(label)}</bs-chip>`;
  }

  #reviewPanel(next) {
    const d = this.#state.detection;
    // dateAtTime ends in "a.m." or "p.m.", which is already the full stop.
    const by = d.review ? ` by ${escapeHTML(d.review.by)}, ${escapeHTML(dateAtTime(d.review.at))}` : ".";
    const article = /^[aeiou]/i.test(d.commonName) ? "an" : "a";
    const verdict = {
      unreviewed: `Listen to the clip. Is this ${article} ${escapeHTML(d.commonName)}?`,
      confirmed: `Confirmed${by}`,
      rejected: `Discarded${by}`,
    }[d.reviewStatus];
    const busy = this.#saving ? "disabled" : "";
    const label = (status, idle, doing) => (this.#saving === status ? doing : idle);

    return `
      <h3>Review</h3>
      <p class="verdict">${verdict ?? ""}</p>
      <div class="row">
        <button class="btn btn--forest btn--small" data-action="review" data-status="confirmed"
                aria-pressed="${d.reviewStatus === "confirmed"}" ${busy}>${label("confirmed", "Confirm", "Confirming…")}</button>
        <button class="btn btn--quiet btn--danger btn--small" data-action="review" data-status="rejected"
                aria-pressed="${d.reviewStatus === "rejected"}" ${busy}>${label("rejected", "Discard", "Discarding…")}</button>
        ${
          d.reviewStatus !== "unreviewed"
            ? `<button class="btn btn--quiet btn--small" data-action="review" data-status="unreviewed" ${busy}>${label("unreviewed", "Undo", "Undoing…")}</button>`
            : ""
        }
      </div>
      ${this.#saveError ? `<p class="error" role="alert">Couldn't save that: ${escapeHTML(this.#saveError)}</p>` : ""}
      ${d.reviewStatus !== "unreviewed" && next ? `<a class="next" href="${escapeHTML(this.#href(next.id))}">Next detection →</a>` : ""}
      <p class="note">Only confirmed detections appear on the public page. Discarded ones are kept, to measure BirdNET against.</p>
    `;
  }

  /** Redraws what a verdict changes, leaving the clip alone. */
  #renderReview() {
    if (this.#state.status !== "ready") return;
    const { next } = this.#neighbours();
    const panel = this.$(".review");
    const chip = this.$(".chip-slot");
    if (panel) panel.innerHTML = this.#reviewPanel(next);
    if (chip) chip.innerHTML = this.#chip();
  }
}

customElements.define("bs-detection-detail", DetectionDetail);
