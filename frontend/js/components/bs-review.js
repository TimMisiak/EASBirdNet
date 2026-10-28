import { BaseElement, escapeHTML } from "./base-element.js";
import { panels, tables, typography } from "../shared-styles.js";
import { count, dateRange } from "../format.js";
import * as api from "../api.js";
import "./bs-progress-bar.js";

/**
 * <bs-review> -- the Review tab: every card with bouts to hear, and how far
 * each one's review has got. A card opens its review, /app/review/{ref}.
 *
 * A card has bouts once BirdNET has been over every file of it and the review
 * rules are saved (REVIEW.md, Preparing a card), so an empty list says which
 * of those is missing rather than nothing.
 */
class Review extends BaseElement {
  static styles = [typography, panels, tables];

  #state = { status: "loading" };

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  async #load() {
    try {
      const { cards } = await api.fetchReviewCards();
      this.#state = { status: "ready", cards };
    } catch (error) {
      this.#state = { status: "error", error };
    }
    if (this.isConnected) this.render();
  }

  render() {
    const { status, error, cards } = this.#state;
    this.shadowRoot.innerHTML = `
      <style>
        .lead { max-width: 44rem; margin: var(--bs-space-2) 0 var(--bs-space-6); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        td.progress { min-width: 12rem; }
        td.progress bs-progress-bar { margin-bottom: var(--bs-space-1); }
        td.progress small { color: var(--bs-text-muted); font-size: 0.78125rem; }
        .ref { font-family: var(--bs-font-mono); font-size: 0.8125rem; }
        .disputed { color: var(--bs-chip-attention-text); }
      </style>

      <h1>Review</h1>
      <p class="lead">Each card's detections are grouped into bouts: one species, heard on and off. Listen to a
      bout's best clips and say whether it's that bird. Two reviewers who agree settle a bout, and their
      verdict counts for every detection in it.</p>
      ${
        status === "loading"
          ? `<p class="empty">Loading the cards…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load the cards: ${escapeHTML(error.message)}</p>`
            : cards.length === 0
              ? `<div class="panel"><p>No cards have bouts to review yet. A card gets them once BirdNET has
                 finished with it and a birding expert has saved the review rules.</p></div>`
              : this.#table(cards)
      }
    `;
  }

  #table(cards) {
    return `
      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Recorder</th>
              <th scope="col">Recorded</th>
              <th scope="col">Card</th>
              <th scope="col">Settled</th>
            </tr>
          </thead>
          <tbody>
            ${cards
              .map((c) => {
                const href = `/app/review/${encodeURIComponent(c.reference)}`;
                const settled = c.bouts ? (c.resolved / c.bouts) * 100 : 0;
                return `
            <tr>
              <td><a href="${escapeHTML(href)}">${escapeHTML(c.stationName || c.stationId)}</a></td>
              <td>${escapeHTML(dateRange(c.firstRecordingAt, c.lastRecordingAt))}</td>
              <td><span class="ref">${escapeHTML(c.reference)}</span></td>
              <td class="progress">
                <bs-progress-bar size="thin" value="${settled}"
                  label="${escapeHTML(`${c.reference}: ${count(c.resolved)} of ${count(c.bouts)} bouts settled`)}"></bs-progress-bar>
                <small>${count(c.resolved)} of ${count(c.bouts)} bouts${
                  c.disputed ? ` · <span class="disputed">${count(c.disputed)} disputed</span>` : ""
                }</small>
              </td>
            </tr>`;
              })
              .join("")}
          </tbody>
        </table>
      </div>
    `;
  }
}

customElements.define("bs-review", Review);
