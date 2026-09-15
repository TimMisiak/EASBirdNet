import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, typography } from "../shared-styles.js";
import { byteSize, count, duration, longDate, megabits, minutesLeft, percent } from "../format.js";
import { navigate } from "../router.js";
import * as flow from "../upload-flow.js";
import "./bs-progress-bar.js";
import "./bs-upload-file-list.js";

/**
 * <bs-upload-progress> -- step 3, and the interrupted screen that shares its
 * state. They are one component because they are one situation: a card part-way
 * across, with a count that has to agree in both views.
 *
 * Whatever a card's progress, the screen never implies work has been lost --
 * the server counts a file once it has all of it, so "we stopped at 214 of 336"
 * is a statement of fact, not an estimate.
 *
 * Progress arrives several times a second, so the page is only rebuilt when the
 * situation changes (sending, or stopped). In between, the numbers and the file
 * list are updated in place.
 */
class UploadProgress extends BaseElement {
  static styles = [typography, controls, forms, panels];

  #unsubscribe = null;
  #wakeLock = null;
  #view = null;
  #onVisible = null;

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = flow.subscribe((state) => {
      if (state.status === "done") {
        navigate("/app/upload/done", { replace: true });
        return;
      }
      this.#syncWakeLock(state.status);
      if (state.upload && viewOf(state) === this.#view) this.#update(state);
      else this.render();
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

  get actions() {
    return {
      pause: () => (flow.get().status === "paused" ? flow.start() : flow.pause()),
      retry: () => flow.start(),
      rechoose: () => navigate("/app/upload"),
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
    if (!state.upload) {
      this.#view = null;
      this.shadowRoot.innerHTML = `<p class="lede">Finding the card…</p>`;
      return;
    }
    this.#view = viewOf(state);
    this.shadowRoot.innerHTML = this.#view === "sending" ? this.#sending(state) : this.#stopped(state);
    this.#update(state);
  }

  /** Everything that moves while the page stands still. */
  #update(state) {
    const { upload, status, files } = state;
    const sent = flow.totals();
    const done = percent(sent.bytes, upload.totalBytes);
    const speed = flow.bytesPerSecond();

    this.#out("pct", `${Math.floor(done)}%`);
    this.#out(
      "remain",
      status === "paused"
        ? `Paused · ${count(upload.fileCount - sent.files)} files left`
        : minutesLeft(flow.minutesRemaining()),
    );
    this.#out("files", `${count(sent.files)} / ${count(upload.fileCount)}`);
    this.#out("bytes", byteSize(sent.bytes));
    this.#out("elapsed", duration(flow.minutesElapsed()));
    this.#out("speed", status !== "uploading" ? "—" : speed ? megabits(speed) : "measuring…");
    this.#out("pause", status === "paused" ? "Resume upload" : "Pause upload");

    const bar = this.$('bs-progress-bar[data-out="bar"]');
    const value = String(Math.round(done * 10) / 10);
    if (bar && bar.getAttribute("value") !== value) bar.setAttribute("value", value);

    const list = this.$("bs-upload-file-list");
    if (list) list.files = files;
  }

  #out(key, text) {
    const el = this.$(`[data-out="${key}"]`);
    if (el && el.textContent !== text) el.textContent = text;
  }

  #sending(state) {
    const { upload } = state;
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
            <div class="pct" data-out="pct"></div>
            <div class="remain" data-out="remain"></div>
          </div>
          <bs-progress-bar data-out="bar" label="Card upload"></bs-progress-bar>

          <div class="live">
            ${live("files", "files uploaded")}
            ${live("bytes", `of ${byteSize(upload.totalBytes)}`)}
            ${live("elapsed", "elapsed")}
            ${live("speed", "current speed")}
          </div>

          ${FILES_HEAD}
          <bs-upload-file-list></bs-upload-file-list>
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
            <button class="btn btn--quiet btn--small btn--block" data-action="pause" data-out="pause"></button>
          </div>
        </div>
      </div>
    `;
  }

  #stopped(state) {
    const { upload, files, error } = state;
    // After a reload the tab no longer has the card's files, so carrying on
    // means choosing the card again.
    const inHand = files.length > 0;
    const sent = flow.totals();
    const filesLeft = upload.fileCount - sent.files;

    return `
      ${STYLE}
      <div class="narrow">
        <div class="eyebrow" style="color: var(--bs-amber-ink); margin-bottom: 0.875rem;">
          Interrupted — your progress is saved
        </div>
        <h1 style="margin-bottom: var(--bs-space-3);">
          We stopped at ${count(sent.files)} of ${count(upload.fileCount)} files.
        </h1>
        <p class="lede" style="font-size: 0.96875rem; margin-bottom: 1.75rem;">
          Nothing is lost. Every file that made it is checked off on our side.
          ${
            inHand
              ? `When you're ready, carry on and we'll upload only the ${count(filesLeft)} files that are still missing.`
              : `To carry on, choose the card again and we'll upload only the ${count(filesLeft)} files that are still missing.`
          }
        </p>
        ${error ? `<p class="error" style="margin-bottom: 1.75rem;">${escapeHTML(error.message)}</p>` : ""}

        <bs-progress-bar value="${percent(sent.files, upload.fileCount)}" label="Files uploaded so far"></bs-progress-bar>
        <div class="split">
          <span>${count(sent.files)} files · ${byteSize(sent.bytes)} uploaded</span>
          <span>${count(filesLeft)} files · ${byteSize(upload.totalBytes - sent.bytes)} remaining</span>
        </div>

        <div class="row" style="margin-bottom: 2.125rem;">
          ${
            inHand
              ? `<button class="btn btn--primary" data-action="retry">Try again now</button>`
              : `<button class="btn btn--primary" data-action="rechoose">Choose the card again</button>`
          }
          <button class="btn btn--quiet" data-action="later">Finish later</button>
        </div>

        ${inHand ? `${FILES_HEAD}<bs-upload-file-list style="margin-bottom: 2.125rem;"></bs-upload-file-list>` : ""}

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

/** Which screen a state gets: files moving (or paused), or stopped. */
function viewOf(state) {
  return state.files.length && state.status !== "interrupted" ? "sending" : "stopped";
}

function live(key, label) {
  return `
    <div>
      <div class="live-value" data-out="${key}"></div>
      <div class="live-label">${escapeHTML(label)}</div>
    </div>
  `;
}

const FILES_HEAD = `
  <div class="files-head">
    <h3>Files</h3>
    <span class="note">One at a time, in the order they're on the card</span>
  </div>
`;

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

    .files-head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--bs-space-4); flex-wrap: wrap; margin-bottom: var(--bs-space-3); }
    .files-head h3 { font-size: 1.125rem; }

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
    }
  </style>
`;

customElements.define("bs-upload-progress", UploadProgress);
