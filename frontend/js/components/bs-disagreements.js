import { BaseElement, escapeHTML } from "./base-element.js";
import { panels, tables, typography } from "../shared-styles.js";
import { count, dateAtTime } from "../format.js";
import * as api from "../api.js";
import { verdictLabel } from "../review-queue.js";

/**
 * <bs-disagreements> -- every card's disputed bouts, for a birding expert: the
 * bouts whose three listeners didn't agree, with what each of them said. A
 * bout opens in its card's review, where the expert hears it and gives the
 * final say, which settles it for every detection in it.
 */
class Disagreements extends BaseElement {
  static styles = [typography, panels, tables];

  #state = { status: "loading" };

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  async #load() {
    try {
      const { disagreements } = await api.fetchDisagreements();
      this.#state = { status: "ready", disagreements };
    } catch (error) {
      this.#state = { status: "error", error };
    }
    if (this.isConnected) this.render();
  }

  render() {
    const { status, error, disagreements } = this.#state;
    this.shadowRoot.innerHTML = `
      <style>
        .lead { max-width: 44rem; margin: var(--bs-space-2) 0 var(--bs-space-6); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
        .sci { font-family: var(--bs-font-display); font-style: italic; color: var(--bs-text-muted); font-size: 0.875rem; }
        .verdicts { margin: 0; padding: 0; list-style: none; font-size: 0.84375rem; }
        .verdicts li { white-space: nowrap; }
        small { display: block; color: var(--bs-text-muted); font-size: 0.78125rem; }
      </style>

      <h1>Disagreements</h1>
      <p class="lead">Bouts whose three listeners didn't agree. Open one to hear it and give the final say,
      which settles it for every detection in it.</p>
      ${
        status === "loading"
          ? `<p class="empty">Loading the disagreements…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load the disagreements: ${escapeHTML(error.message)}</p>`
            : disagreements.length === 0
              ? `<div class="panel"><p>Nothing is disputed: every bout heard so far has listeners who agree.</p></div>`
              : this.#table(disagreements)
      }
    `;
  }

  #table(disagreements) {
    return `
      <p class="note">${count(disagreements.length)} disputed ${disagreements.length === 1 ? "bout" : "bouts"}</p>
      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Species</th>
              <th scope="col">Heard</th>
              <th scope="col">Recorder</th>
              <th scope="col">Its listeners said</th>
            </tr>
          </thead>
          <tbody>
            ${disagreements
              .map(({ card, bout: b }) => {
                const href = `/app/review/${encodeURIComponent(card.reference)}?bout=${encodeURIComponent(b.id)}&from=disagreements`;
                return `
            <tr>
              <td>
                <a href="${escapeHTML(href)}">${escapeHTML(b.commonName)}</a>
                ${b.scientificName !== b.commonName ? `<span class="sci">${escapeHTML(b.scientificName)}</span>` : ""}
                <small>${b.kind === "spotcheck" ? "Spot-check" : "Bout"} of ${b.detections === 1 ? "1 detection" : `${count(b.detections)} detections`}</small>
              </td>
              <td>${escapeHTML(dateAtTime(b.firstHeardAt))}</td>
              <td>${escapeHTML(card.stationName || card.stationId)}<small>${escapeHTML(card.reference)}</small></td>
              <td>
                <ul class="verdicts">
                  ${(b.verdicts ?? []).map((v) => `<li>${escapeHTML(v.by)}: ${escapeHTML(verdictLabel(v, b))}</li>`).join("")}
                </ul>
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

customElements.define("bs-disagreements", Disagreements);
