import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, panels, typography } from "../shared-styles.js";
import { count, duration, gigabytes, longDate, minutesLeft, percent, shortDate } from "../format.js";
import { navigate } from "../router.js";
import * as flow from "../upload-flow.js";
import "./bs-progress-bar.js";

/**
 * <bs-upload-progress> -- step 3, and the interrupted screen that shares its
 * state. They are one component because they are one situation: a card part-way
 * across, with a count that has to agree in both views.
 *
 * Whatever a card's progress, the screen never implies work has been lost --
 * the server's count is authoritative and only moves forward, so "we stopped at
 * 214 of 336" is a statement of fact, not an estimate.
 */
class UploadProgress extends BaseElement {
  static styles = [typography, controls, panels];

  #unsubscribe = null;
  #wakeLock = null;

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = flow.subscribe((state) => {
      if (state.status === "done") {
        navigate("/app/upload/done", { replace: true });
        return;
      }
      this.#syncWakeLock(state.status);
      this.render();
    });
    flow.current().then((upload) => {
      if (!upload) navigate("/app/upload", { replace: true });
      else this.render();
    });
    // The browser drops a wake lock whenever the tab is hidden; take it back.
    this.#onVisible = () => this.#syncWakeLock(flow.get().status);
    document.addEventListener("visibilitychange", this.#onVisible);
  }

  disconnectedCallback() {
    this.#unsubscribe?.();
    document.removeEventListener("visibilitychange", this.#onVisible);
    this.#releaseWakeLock();
  }

  #onVisible = null;

  get actions() {
    return {
      pause: () => (flow.get().status === "paused" ? flow.start() : flow.pause()),
      retry: () => flow.start(),
      later: () => navigate("/app"),
    };
  }

  /** Hold the screen awake while bytes are moving, and only then. */
  async #syncWakeLock(status) {
    const wanted = status === "uploading" && document.visibilityState === "visible";
    if (wanted && !this.#wakeLock && navigator.wakeLock) {
      this.#wakeLock = await navigator.wakeLock.request("screen").catch(() => null);
    } else if (!wanted) {
      this.#releaseWakeLock();
    }
  }

  #releaseWakeLock() {
    this.#wakeLock?.release().catch(() => {});
    this.#wakeLock = null;
  }

  render() {
    const state = flow.get();
    const { upload } = state;
    if (!upload) {
      this.shadowRoot.innerHTML = `<p class="lede">Finding the card…</p>`;
      return;
    }
    this.shadowRoot.innerHTML =
      state.status === "interrupted" ? this.#interrupted(state) : this.#uploading(state);
  }

  #uploading(state) {
    const { upload, filesUploaded, bytesUploaded, status } = state;
    const done = percent(bytesUploaded, upload.totalBytes);
    const remaining = upload.fileCount - Math.round(filesUploaded);

    return `
      ${STYLE}
      <h1>Uploading the card</h1>
      <p class="lede" style="margin-bottom: 1.875rem;">
        ${escapeHTML(upload.stationName)} · ${escapeHTML(upload.stationId)} ·
        card pulled ${escapeHTML(longDate(upload.pulledOn))}
      </p>

      <div class="columns">
        <div>
          <div class="headline">
            <div class="pct">${Math.round(done)}%</div>
            <div class="remain">${
              status === "paused"
                ? `Paused · ${count(remaining)} files left`
                : escapeHTML(minutesLeft(flow.minutesRemaining()))
            }</div>
          </div>
          <bs-progress-bar value="${done}" label="Card upload"></bs-progress-bar>

          <div class="live">
            ${live(`${count(Math.round(filesUploaded))} / ${count(upload.fileCount)}`, "files uploaded")}
            ${live(gigabytes(bytesUploaded), `of ${gigabytes(upload.totalBytes)}`)}
            ${live(duration(flow.minutesElapsed()), "elapsed")}
            ${live(flow.linkSpeed(), "current speed")}
          </div>

          <div class="nights">
            ${flow
              .nightProgress()
              .map(
                (night) => `
              <div class="night">
                <span class="dot" data-state="${night.percent >= 100 ? "done" : night.percent > 0 ? "active" : "waiting"}"></span>
                <span class="night-date">${escapeHTML(shortDate(night.date))}</span>
                <bs-progress-bar size="thin" value="${night.percent}"
                                 label="${escapeHTML(shortDate(night.date))}"></bs-progress-bar>
                <span class="night-count">${count(night.done)} / ${count(night.files)} files</span>
              </div>`,
              )
              .join("")}
          </div>
        </div>

        <div class="aside">
          <div class="panel panel--notice">
            <h3>Leave this tab open</h3>
            <p>
              Don't close the tab or let the laptop sleep. Other apps are fine. We're keeping
              the screen awake while this tab is visible.
            </p>
          </div>
          <div class="panel">
            <h3>Progress is saved</h3>
            <p style="margin-bottom: var(--bs-space-4);">
              Each file is checked off as it lands. If anything interrupts this, come back and
              we'll finish only what's missing.
            </p>
            <button class="btn btn--quiet btn--small btn--block" data-action="pause">
              ${status === "paused" ? "Resume upload" : "Pause upload"}
            </button>
          </div>
        </div>
      </div>
    `;
  }

  #interrupted(state) {
    const { upload, filesUploaded, bytesUploaded } = state;
    const done = percent(filesUploaded, upload.fileCount);
    const filesLeft = upload.fileCount - Math.round(filesUploaded);

    return `
      ${STYLE}
      <div class="narrow">
        <div class="eyebrow" style="color: var(--bs-amber-ink); margin-bottom: 0.875rem;">
          Interrupted — your progress is saved
        </div>
        <h1 style="margin-bottom: var(--bs-space-3);">
          We stopped at ${count(Math.round(filesUploaded))} of ${count(upload.fileCount)} files.
        </h1>
        <p class="lede" style="font-size: 0.96875rem; margin-bottom: 1.75rem;">
          Nothing is lost. Every file that made it is checked off on our side. When you're
          ready, carry on and we'll upload only the ${count(filesLeft)} files that are still
          missing.
        </p>

        <bs-progress-bar value="${done}" label="Files uploaded so far"></bs-progress-bar>
        <div class="split">
          <span>${count(Math.round(filesUploaded))} files · ${gigabytes(bytesUploaded)} uploaded</span>
          <span>${count(filesLeft)} files · ${gigabytes(upload.totalBytes - bytesUploaded)} remaining</span>
        </div>

        <div class="row" style="margin-bottom: 2.125rem;">
          <button class="btn btn--primary" data-action="retry">Try again now</button>
          <button class="btn btn--quiet" data-action="later">Finish later</button>
        </div>

        <div class="panel">
          <h3>The usual causes</h3>
          <ul>
            <li>The laptop went to sleep — plug it in and set it to stay awake</li>
            <li>Wi-Fi dropped — moving closer to the router or plugging in ethernet helps</li>
            <li>The card reader was unplugged or the card was removed</li>
          </ul>
          <p class="note" style="margin-top: var(--bs-space-4);">
            Keep the card until you get the “card received” email. Reference
            <span class="mono" style="color: var(--bs-text);">${escapeHTML(upload.reference)}</span>.
          </p>
        </div>
      </div>
    `;
  }
}

function live(value, label) {
  return `
    <div>
      <div class="live-value">${escapeHTML(value)}</div>
      <div class="live-label">${escapeHTML(label)}</div>
    </div>
  `;
}

const STYLE = `
  <style>
    .narrow { max-width: 760px; }
    .columns {
      display: grid;
      grid-template-columns: minmax(0, 1.25fr) minmax(0, 0.75fr);
      gap: 2.25rem;
      align-items: start;
    }
    .headline { display: flex; align-items: baseline; justify-content: space-between; gap: var(--bs-space-4); margin-bottom: var(--bs-space-3); }
    .pct { font-family: var(--bs-font-display); font-size: 2.875rem; line-height: 1; font-variant-numeric: tabular-nums; }
    .remain { font-size: 0.875rem; color: var(--bs-text-body); }

    .live { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: var(--bs-space-4); margin: 1.375rem 0 1.875rem; }
    .live-value { font-family: var(--bs-font-mono); font-size: 1.125rem; }
    .live-label { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.25rem; }

    .nights { display: flex; flex-direction: column; }
    .night {
      display: flex;
      align-items: center;
      gap: var(--bs-space-3);
      padding: var(--bs-space-2) 0;
      border-top: 1px solid var(--bs-rule);
      font-size: 0.875rem;
    }
    .night bs-progress-bar { flex: 1; }
    .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; background: var(--bs-border-strong); }
    .dot[data-state="active"] { background: var(--bs-amber); }
    .dot[data-state="done"] { background: var(--bs-forest); }
    .night-date { width: 74px; flex: none; }
    .night-count { font-family: var(--bs-font-mono); font-size: 0.75rem; color: var(--bs-text-muted); width: 6.5rem; text-align: right; flex: none; }

    .aside { display: flex; flex-direction: column; gap: var(--bs-space-4); }
    .aside ul, .panel ul {
      margin: 0;
      padding-left: 1.125rem;
      display: flex;
      flex-direction: column;
      gap: var(--bs-space-2);
      font-size: 0.875rem;
      line-height: 1.55;
      color: var(--bs-text-soft);
    }
    .split { display: flex; justify-content: space-between; gap: var(--bs-space-4); flex-wrap: wrap; font-size: 0.8125rem; color: var(--bs-text-muted); margin: var(--bs-space-2) 0 1.875rem; }
    .row { display: flex; gap: var(--bs-space-3); flex-wrap: wrap; }

    @media (max-width: 860px) {
      .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); }
      .live { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .night-count { width: auto; }
    }
  </style>
`;

customElements.define("bs-upload-progress", UploadProgress);
