import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, panels, tables, typography } from "../shared-styles.js";
import { count, longDate, percent } from "../format.js";
import { isUnfinished, statusChip } from "../upload-status.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as session from "../session.js";
import * as flow from "../upload-flow.js";
import "./bs-chip.js";
import "./bs-progress-bar.js";

/**
 * <bs-volunteer-home> is where a signed-in volunteer lands. It answers one
 * question first -- is there a card I left half-finished? -- and offers the new
 * upload second, because an abandoned card is the thing that costs the program
 * a season of audio.
 */
class VolunteerHome extends BaseElement {
  static styles = [typography, controls, panels, tables];

  #state = { status: "loading", uploads: [], error: null };

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  get actions() {
    return {
      resume: async (el) => {
        await flow.adopt(el.dataset.reference);
        // A card this tab is already sending carries on where it is. Any other
        // needs the card chosen again, so the server can say what's missing.
        navigate(flow.get().files.length ? "/app/upload/progress" : "/app/upload");
      },
      start: () => {
        flow.reset();
        navigate("/app/upload");
      },
    };
  }

  async #load() {
    try {
      const { uploads } = await api.fetchMyUploads();
      this.#state = { status: "ready", uploads, error: null };
    } catch (error) {
      this.#state = { status: "error", uploads: [], error };
    }
    if (this.isConnected) this.render();
  }

  render() {
    const { status, uploads, error } = this.#state;
    const unfinished = uploads.find(isUnfinished);

    this.shadowRoot.innerHTML = `
      <style>
        .greeting {
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: var(--bs-space-5);
          flex-wrap: wrap;
          margin-bottom: 2.125rem;
        }
        .greeting h1 { margin-bottom: 0.375rem; }
        .callout {
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: 1.75rem;
          flex-wrap: wrap;
          margin-bottom: var(--bs-space-5);
        }
        .callout .eyebrow { color: var(--bs-amber-ink); margin-bottom: var(--bs-space-2); }
        .callout-title { font-family: var(--bs-font-display); font-size: 1.5rem; margin-bottom: 0.375rem; }
        .callout-detail { font-size: 0.875rem; color: var(--bs-text-body); }
        bs-progress-bar { margin-top: 0.875rem; width: 320px; max-width: 100%; }
        .new { margin-bottom: 2.75rem; }
        h2 { margin-bottom: var(--bs-space-1); }
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; white-space: nowrap; }
        /* Every cell here is one token -- a reference, a station, a date. On a
           narrow screen the table scrolls inside its own box instead. */
        td, th { padding-top: 0.9375rem; padding-bottom: 0.9375rem; white-space: nowrap; }
        td { font-size: 0.90625rem; }
        /* An unfinished card's "Finish uploading" link covers its row. */
        tbody tr { position: relative; }
        tbody tr[data-unfinished]:hover { background: var(--bs-surface-sunk); }
        .go { text-align: right; }
        .go a { text-decoration: none; }
        .go a::after { content: ""; position: absolute; inset: 0; }
        tbody tr:hover .go a { text-decoration: underline; }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        @media (max-width: 720px) {
          .callout, .new { padding: var(--bs-space-4); }
          .callout .btn, .new .btn { width: 100%; }
        }
      </style>

      <div class="greeting">
        <div>
          <h1>${greeting()}</h1>
          <p class="lede">${
            session.isAdmin()
              ? "You're an admin, which includes everything a volunteer can do."
              : "You're signed in as a volunteer for the owl monitoring program."
          }</p>
        </div>
        ${session.isAdmin() ? `<a href="/admin/people">Program admin →</a>` : ""}
      </div>

      ${unfinished ? this.#unfinishedCard(unfinished) : ""}

      <div class="panel new callout">
        <div>
          <div class="callout-title">Upload a new SD card</div>
          <div class="callout-detail">
            Put the card in your reader first. A full card takes about 2–3 hours.
          </div>
        </div>
        <button class="btn btn--forest" data-action="start">Upload an SD card →</button>
      </div>

      <h2>Your cards</h2>
      ${
        status === "loading"
          ? `<p class="empty">Looking up your cards…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load your cards: ${escapeHTML(error.message)}</p>`
            : uploads.length === 0
              ? `<p class="empty">No cards yet. The first one you upload will show up here.</p>`
              : this.#table(uploads)
      }
    `;
  }

  #unfinishedCard(upload) {
    const done = percent(upload.filesUploaded, upload.fileCount);
    return `
      <div class="panel panel--notice callout">
        <div style="min-width: 0;">
          <div class="eyebrow">Unfinished upload</div>
          <div class="callout-title">${escapeHTML(upload.stationName)} · ${escapeHTML(upload.stationId)}</div>
          <div class="callout-detail">
            ${count(upload.filesUploaded)} of ${count(upload.fileCount)} files uploaded ·
            card pulled ${escapeHTML(longDate(upload.pulledOn))}
          </div>
          <bs-progress-bar value="${done}" label="Files uploaded so far"></bs-progress-bar>
        </div>
        <button class="btn btn--primary" data-action="resume"
                data-reference="${escapeHTML(upload.reference)}">Resume upload →</button>
      </div>
    `;
  }

  #table(uploads) {
    return `
      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Reference</th>
              <th scope="col">Station</th>
              <th scope="col">Pulled</th>
              <th scope="col" style="text-align: right;">Nights</th>
              <th scope="col">Status</th>
              <th scope="col" aria-label="Next step"></th>
            </tr>
          </thead>
          <tbody>
            ${uploads
              .map((u) => {
                const chip = statusChip(u);
                const unfinished = isUnfinished(u);
                return `
                  <tr ${unfinished ? "data-unfinished" : ""}>
                    <td class="ref">${escapeHTML(u.reference)}</td>
                    <td>${escapeHTML(u.stationName)}</td>
                    <td style="color: var(--bs-text-body);">${escapeHTML(longDate(u.pulledOn))}</td>
                    <td class="num">${u.nights?.length ?? 0}</td>
                    <td><bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip></td>
                    <td class="go">${
                      unfinished
                        ? `<a href="/app/upload" data-action="resume"
                              data-reference="${escapeHTML(u.reference)}">Finish uploading →</a>`
                        : ""
                    }</td>
                  </tr>`;
              })
              .join("")}
          </tbody>
        </table>
      </div>
    `;
  }
}

/** Volunteers swap cards early; the greeting should match the hour they're in. */
function greeting() {
  const hour = new Date().getHours();
  if (hour < 12) return "Good morning.";
  if (hour < 18) return "Good afternoon.";
  return "Good evening.";
}

customElements.define("bs-volunteer-home", VolunteerHome);
