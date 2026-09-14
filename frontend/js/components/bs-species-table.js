import { BaseElement, escapeHTML } from "./base-element.js";
import { tables, typography } from "../shared-styles.js";
import { count, dateAtTime } from "../format.js";

/**
 * <bs-species-table> lists what has been confirmed, one row per species. Set
 * the `species` property with the array from the API.
 *
 * At narrow widths the table restacks into one line per species rather than
 * scrolling sideways: the count is the thing being scanned for, and it should
 * stay next to the name.
 */
class SpeciesTable extends BaseElement {
  static styles = [typography, tables];

  #species = [];

  set species(value) {
    this.#species = value ?? [];
    if (this.isConnected) this.render();
  }

  get species() {
    return this.#species;
  }

  render() {
    this.shadowRoot.innerHTML = `
      <style>
        .name { font-family: var(--bs-font-display); font-size: 1.3125rem; }
        .latin { font-size: 0.78125rem; color: var(--bs-text-muted); font-style: italic; margin-top: 0.125rem; }
        td { padding-top: 1.125rem; padding-bottom: 1.125rem; }
        .count { font-size: 1rem; }
        .nights { color: var(--bs-text-muted); }
        .stations, .last { font-size: 0.875rem; color: var(--bs-text-soft); }
        .last { white-space: nowrap; }
        .sub { display: none; }

        @media (max-width: 760px) {
          thead, .nights, .stations, .last { display: none; }
          table, tbody, tr, td { display: block; }
          tr {
            display: grid;
            grid-template-columns: minmax(0, 1fr) auto;
            align-items: baseline;
            gap: var(--bs-space-3);
            border-top: none;
            border-bottom: 1px solid var(--bs-border);
            padding: 0.875rem 0;
          }
          td { padding: 0; }
          .name { font-size: 1.125rem; }
          .sub { display: block; font-size: 0.71875rem; color: var(--bs-text-muted); margin-top: 0.1875rem; }
          .count { text-align: right; }
        }
      </style>
      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Species</th>
              <th scope="col" style="text-align: right;">Detections</th>
              <th scope="col" style="text-align: right;">Nights heard</th>
              <th scope="col">Stations</th>
              <th scope="col">Most recent</th>
            </tr>
          </thead>
          <tbody>
            ${this.#species.map(row).join("")}
          </tbody>
        </table>
      </div>
    `;
  }
}

function row(s) {
  const stations = (s.stations ?? []).map(shortName);
  return `
    <tr>
      <td>
        <div class="name">${escapeHTML(s.commonName)}</div>
        <div class="latin">${escapeHTML(s.scientificName)}</div>
        <div class="sub">${escapeHTML(`${s.nights} nights · ${stations.join(", ")}`)}</div>
      </td>
      <td class="num count">${count(s.detections)}</td>
      <td class="num nights">${count(s.nights)}</td>
      <td class="stations">${escapeHTML(stations.join(", "))}</td>
      <td class="last">${escapeHTML(dateAtTime(s.lastDetectedAt))}</td>
    </tr>
  `;
}

/** "Marymoor Park – Snag Row" is the station; "Marymoor" is the place. */
const shortName = (name) => name.split(" – ")[0].replace(/ Park$/, "");

customElements.define("bs-species-table", SpeciesTable);
