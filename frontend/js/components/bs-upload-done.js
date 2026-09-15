import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, panels, typography } from "../shared-styles.js";
import { byteSize, count, longDate, nightRange } from "../format.js";
import { navigate } from "../router.js";
import * as flow from "../upload-flow.js";

/**
 * <bs-upload-done> -- step 4. The volunteer is here to learn one thing, which
 * is the headline: the card can be erased and put back in the rotation. The
 * summary underneath is the same set of facts the confirmation email carries,
 * so the two can be checked against each other.
 */
class UploadDone extends BaseElement {
  static styles = [typography, controls, panels];

  connectedCallback() {
    super.connectedCallback();
    flow.current().then((upload) => {
      if (!upload) navigate("/app", { replace: true });
      else this.render();
    });
  }

  get actions() {
    return {
      home: () => {
        flow.reset();
        navigate("/app/uploads");
      },
      again: () => {
        flow.reset();
        navigate("/app/upload");
      },
    };
  }

  render() {
    const { upload } = flow.get();
    if (!upload) {
      this.shadowRoot.innerHTML = `<p class="lede">One moment…</p>`;
      return;
    }
    const nights = upload.nights ?? [];

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; max-width: 820px; }
        .received { display: flex; align-items: center; gap: 0.875rem; margin-bottom: 1.125rem; }
        .tick {
          width: 34px; height: 34px;
          border-radius: 50%;
          background: var(--bs-forest);
          color: var(--bs-on-forest);
          display: inline-flex;
          align-items: center;
          justify-content: center;
          font-size: 1.0625rem;
          flex: none;
        }
        .received .eyebrow { color: var(--bs-forest); letter-spacing: 0.18em; }
        h1 { font-size: clamp(2rem, 1.4rem + 2vw, 2.5rem); margin-bottom: 0.875rem; }
        .intro { font-size: 1rem; line-height: 1.6; color: var(--bs-text-body); margin-bottom: var(--bs-space-6); max-width: 64ch; }
        .summary { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 1.625rem var(--bs-space-6); }
        .summary .eyebrow { margin-bottom: 0.4375rem; }
        .summary dd { margin: 0; font-size: 0.96875rem; }
        dl { margin: 0; }
        .panel { margin-bottom: var(--bs-space-5); }
        .next { margin-bottom: 2.125rem; }
        @media (max-width: 720px) { .summary { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
        @media (max-width: 480px) { .summary { grid-template-columns: minmax(0, 1fr); } }
      </style>

      <div class="received">
        <span class="tick" aria-hidden="true">✓</span>
        <span class="eyebrow">All ${count(upload.fileCount)} files received</span>
      </div>
      <h1>The card is safe to erase.</h1>
      <p class="intro">
        Everything from ${escapeHTML(upload.stationName)} is in our storage and checked
        against what was on the card. Erase it and put it back in the rotation. We've
        emailed you a copy of this summary.
      </p>

      <div class="panel">
        <dl class="summary">
          ${entry("Reference", upload.reference)}
          ${entry("Station", upload.stationName)}
          ${entry("Recorder", upload.stationId)}
          ${entry("Nights", `${count(nights.length)} · ${nightRange(nights)}`)}
          ${entry("Files received", `${count(upload.filesUploaded)} of ${count(upload.fileCount)} · ${byteSize(upload.totalBytes)}`)}
          ${entry("Card pulled", longDate(upload.pulledOn))}
          ${upload.notes ? entry("Your note", `“${upload.notes}”`) : ""}
        </dl>
      </div>

      <div class="panel panel--parchment next">
        <h3>What happens next</h3>
        <p style="font-size: 0.90625rem; line-height: 1.65; color: var(--bs-text-soft);">
          BirdNET is running on your ${count(nights.length)} nights now — it usually finishes
          overnight. You'll get a second email with the results for this card, and any owl
          detections go to the review volunteers before they appear on the public page.
        </p>
      </div>

      <div class="row">
        <button class="btn btn--forest" data-action="home">See my uploads</button>
        <button class="btn btn--quiet" data-action="again">Upload another card</button>
      </div>
    `;
  }
}

function entry(label, value) {
  return `
    <div>
      <dt class="eyebrow">${escapeHTML(label)}</dt>
      <dd>${escapeHTML(value)}</dd>
    </div>
  `;
}

customElements.define("bs-upload-done", UploadDone);
