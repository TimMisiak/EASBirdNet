import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, panels, tables, typography } from "../shared-styles.js";
import { byteSize, count, duration, longDate, megabits, nightRange, shortDate } from "../format.js";
import { navigate } from "../router.js";
import * as flow from "../upload-flow.js";
import * as session from "../session.js";
import * as speedTest from "../speed-test.js";

/**
 * <bs-upload-check> -- step 2: what we found on the card, before anything is
 * sent. The point of the screen is the night-by-night table: a volunteer knows
 * whether a short night means flat batteries, and we don't, so the odd nights
 * are highlighted and left for them to explain in the notes.
 *
 * "Test connection" is here, with the card chosen and nothing yet sent, so a
 * volunteer whose uploads crawl can see whether it's the card reader or the
 * line (speed-test.js).
 */
class UploadCheck extends BaseElement {
  static styles = [typography, controls, panels, tables];

  #unsubscribe = null;
  /** The connection test: {running, phase, live, result, error}, or null before one has run. */
  #test = null;
  #abort = null;
  #lastDraw = 0;

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = flow.subscribe(() => this.render());
    const over = () => navigate("/app/upload", { replace: true });
    flow.current().then((upload) => {
      // Reloaded onto this URL, the tab no longer has the card's files: start
      // the step over, which for a registered card means choosing it again.
      if (!upload || !flow.get().files.length) over();
    }, over);
  }

  disconnectedCallback() {
    this.#unsubscribe?.();
    this.#abort?.abort();
  }

  get actions() {
    return {
      back: () => navigate("/app/upload"),
      test: () => this.#runTest(),
      start: () => {
        // The test would be sending beside the card.
        this.#abort?.abort();
        flow.start();
        navigate("/app/upload/progress");
      },
    };
  }

  async #runTest() {
    if (this.#test?.running) return;
    this.#abort = new AbortController();
    this.#test = { running: true, phase: "read", live: null, result: {}, error: null };
    this.render();
    try {
      const result = await speedTest.run(flow.get().files, {
        signal: this.#abort.signal,
        onProgress: ({ phase, bytesPerSecond }) => {
          if (phase !== this.#test.phase) this.#test.result[this.#test.phase] = this.#test.live;
          Object.assign(this.#test, { phase, live: bytesPerSecond });
          // Reading a fast card reports many times a second; the panel doesn't need to.
          if (performance.now() - this.#lastDraw > 250) this.render();
        },
      });
      Object.assign(this.#test, { running: false, phase: null, result });
    } catch (error) {
      if (error?.name === "AbortError") return;
      Object.assign(this.#test, { running: false, phase: null, error });
    }
    if (this.isConnected) this.render();
  }

  render() {
    this.#lastDraw = performance.now();
    const { upload, card } = flow.get();
    if (!upload) {
      this.shadowRoot.innerHTML = `<p class="lede">Reading the card…</p>`;
      return;
    }

    const nights = upload.nights ?? [];
    const flagged = nights.filter((n) => n.flag).length;
    // A resumed card only has what's missing left to send.
    const remaining = upload.totalBytes - upload.bytesUploaded;
    const already = upload.filesUploaded;
    const estimate = duration(flow.totalMinutes(remaining));

    this.shadowRoot.innerHTML = `
      <style>
        .head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--bs-space-5); flex-wrap: wrap; }
        .card-line { font-size: 0.84375rem; color: var(--bs-text-muted); }
        .card-line .mono { color: var(--bs-text); }
        .stats {
          display: grid;
          grid-template-columns: repeat(4, minmax(0, 1fr));
          gap: var(--bs-space-4);
          margin: 1.75rem 0 var(--bs-space-6);
        }
        .stat { background: var(--bs-surface); border: 1px solid var(--bs-border); padding: 1.125rem 1.25rem; }
        .stat-value { font-family: var(--bs-font-display); font-size: 1.875rem; line-height: 1.1; }
        .stat-label { font-size: 0.78125rem; color: var(--bs-text-muted); margin-top: 0.375rem; }

        .columns {
          display: grid;
          grid-template-columns: minmax(0, 1.25fr) minmax(0, 0.75fr);
          gap: 2.25rem;
          align-items: start;
        }
        th { padding-top: 0; padding-bottom: 0.625rem; }
        td { padding: 0.625rem var(--bs-space-3); font-size: 0.875rem; }
        /* The date and the size are single tokens: on a narrow screen the
           table scrolls inside its own box rather than shredding them. */
        td:not(.check), th:not(:last-child) { white-space: nowrap; }
        tbody tr[data-flagged="true"] { background: var(--bs-notice); }
        .check { color: var(--bs-text-soft); }
        .summary { display: flex; flex-direction: column; gap: var(--bs-space-4); }
        .summary .panel > div { font-size: 0.875rem; line-height: 1.5; color: var(--bs-text-soft); }
        .summary .stack { gap: 0.5625rem; }
        .speeds { display: grid; grid-template-columns: 1fr auto; gap: 0.375rem var(--bs-space-4); margin: var(--bs-space-3) 0; font-size: 0.875rem; }
        .speeds dt { color: var(--bs-text-soft); }
        .speeds dd { margin: 0; text-align: right; font-variant-numeric: tabular-nums; }
        .test p { margin-bottom: var(--bs-space-3); }
        @media (max-width: 900px) { .stats { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      <div class="head">
        <h1>Here's what's on the card</h1>
        <div class="card-line">
          Card <span class="mono">${escapeHTML(card?.label ?? upload.stationId)}</span> ·
          ${count(upload.fileCount + (card?.skipped?.length ?? 0))} files read ·
          <a href="/app/upload">Choose a different card</a>
        </div>
      </div>

      <div class="stats">
        ${stat(count(nights.length), `nights · ${nightRange(nights)}`)}
        ${
          already
            ? stat(count(upload.fileCount - already), `of ${count(upload.fileCount)} audio files left`)
            : stat(count(upload.fileCount), "audio files")
        }
        ${stat(byteSize(remaining), upload.bytesUploaded ? "left to upload" : "to upload")}
        ${stat(`~${estimate}`, `at a typical ${flow.assumedSpeed()}`)}
      </div>

      <div class="columns">
        <div>
          <div class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">Night</th>
                  <th scope="col" style="text-align: right;">Files</th>
                  <th scope="col" style="text-align: right;">Size</th>
                  <th scope="col">Check</th>
                </tr>
              </thead>
              <tbody>
                ${nights.map(nightRow).join("")}
              </tbody>
            </table>
          </div>
          <p class="note" style="margin-top: 0.875rem;">
            ${
              card?.skipped?.length
                ? `Skipped ${count(card.skipped.length)} file${card.skipped.length === 1 ? "" : "s"} that aren't audio (${escapeHTML(card.skipped.slice(0, 2).join(", "))}${card.skipped.length > 2 ? ", …" : ""}).`
                : ""
            }
            Nothing on the card is changed or deleted.
          </p>
        </div>

        <div class="summary">
          <div class="panel">
            <h3>This upload</h3>
            <div class="stack">
              <div>${escapeHTML(session.user()?.name ?? "")}</div>
              <div>${escapeHTML(upload.stationName)} · ${escapeHTML(upload.stationId)}</div>
              <div>Card pulled ${escapeHTML(longDate(upload.pulledOn))}</div>
              ${upload.notes ? `<div class="muted">Notes: “${escapeHTML(upload.notes)}”</div>` : `<div class="muted">No notes.</div>`}
              <div class="muted">Reference: <span class="mono" style="color: var(--bs-text);">${escapeHTML(upload.reference)}</span></div>
            </div>
          </div>
          ${
            already
              ? `<div class="panel panel--callout">
                   <h3>Picking up where you left off</h3>
                   <p>
                     ${count(already)} of ${count(upload.fileCount)} files are already uploaded.
                     We'll skip those and send the other ${count(upload.fileCount - already)}.
                   </p>
                 </div>`
              : ""
          }
          ${
            flagged
              ? `<div class="panel panel--notice">
                   <p>
                     ${flagged === 1 ? "One night looks" : `${count(flagged)} nights look`} unusual.
                     That's fine — just check the notes say why, or
                     <a href="/app/upload">edit the details</a>.
                   </p>
                 </div>`
              : ""
          }
          ${this.#testPanel()}
        </div>
      </div>

      <div class="step-footer">
        <button class="btn btn--quiet btn--small" data-action="back">← Back</button>
        <span class="row" style="align-items: center; gap: var(--bs-space-5);">
          <span class="note">Keep the laptop awake and plugged in.</span>
          <button class="btn btn--primary" data-action="start">${already ? "Upload the rest →" : "Start upload →"}</button>
        </span>
      </div>
    `;
  }

  #testPanel() {
    const t = this.#test;
    const value = (key) => {
      if (t?.phase === key) return t.live ? `${megabits(t.live)}…` : "measuring…";
      const bps = t?.result?.[key];
      return bps ? megabits(bps) : "—";
    };
    return `
      <div class="panel test">
        <h3>Test the connection</h3>
        <p>
          Uploads slower than your internet plan? This reads from the card, then sends test
          data to us, for about 25 seconds. Nothing is saved.
        </p>
        ${
          t
            ? `<dl class="speeds" aria-live="polite">
                 ${PHASES.map(([key, label]) => `<dt>${label}${key === "parallel" && t.result?.streams ? ` (${t.result.streams})` : ""}</dt><dd>${value(key)}</dd>`).join("")}
               </dl>`
            : ""
        }
        ${t && !t.running && !t.error ? `<p>${escapeHTML(verdict(t.result))}</p>` : ""}
        ${t?.error ? `<p class="error">${escapeHTML(t.error.message)}</p>` : ""}
        <button class="btn btn--quiet btn--small" data-action="test" ${t?.running ? "disabled" : ""}>
          ${t?.running ? "Testing…" : t ? "Test again" : "Test connection"}
        </button>
      </div>
    `;
  }
}

const PHASES = [
  ["read", "Reading the card"],
  ["single", "Sending, one file at a time"],
  ["parallel", "Sending, several files at once"],
];

/** Which part an upload is waiting on, from a finished test. */
function verdict({ read, parallel }) {
  if (!read || !parallel) return "";
  // Past this, a card is a couple of hours however it splits, and neither is worth chasing.
  if (Math.min(read, parallel) * 8 >= 200e6) {
    return `Both look fast: a card should upload at around ${megabits(Math.min(read, parallel))}.`;
  }
  if (read < parallel * 0.8) {
    return `The card reader is the slower part: the upload can't go faster than ${megabits(read)}, however fast the connection. A USB 3 reader, plugged straight into the computer, usually reads faster.`;
  }
  return `The connection is the slower part: the card reads faster than it can be sent. A wired connection is usually faster than Wi-Fi.`;
}

const REASONS = {
  short: "⚠ Short night — fewer files than the others",
  partial: "⚠ Partial — card was pulled this morning",
};

function nightRow(night) {
  return `
    <tr data-flagged="${Boolean(night.flag)}">
      <td>${escapeHTML(shortDate(night.date))}</td>
      <td class="num" style="font-size: 0.8125rem;">${count(night.files)}</td>
      <td class="num muted" style="font-size: 0.8125rem;">${byteSize(night.bytes)}</td>
      <td class="check">${escapeHTML(REASONS[night.flag] ?? "✓ looks normal")}</td>
    </tr>
  `;
}

function stat(value, label) {
  return `
    <div class="stat">
      <div class="stat-value">${escapeHTML(value)}</div>
      <div class="stat-label">${escapeHTML(label)}</div>
    </div>
  `;
}

customElements.define("bs-upload-check", UploadCheck);
