import { BaseElement, escapeHTML } from "./base-element.js";
import { panels, typography } from "../shared-styles.js";
import { count, stamp } from "../format.js";
import * as api from "../api.js";
import "./bs-species-table.js";

/**
 * <bs-home-page> is the public page: what the recorders heard, and how the
 * program works. It is the only page an unauthenticated visitor sees, so it
 * shows no volunteer names, no card references, and no upload state.
 */

// The four steps are the program's own description of itself, not data -- they
// change when the program changes, not when a card is uploaded.
const STEPS = [
  {
    n: "01",
    title: "Recorders listen",
    body: "A SwiftOne at each station records one-hour files from dusk to dawn, about 14 nights on a card.",
  },
  {
    n: "02",
    title: "Volunteers swap cards",
    body: "Every two weeks a volunteer changes the batteries and the card and uploads it from home.",
  },
  {
    n: "03",
    title: "BirdNET listens back",
    body: "The model scans every night of audio and flags anything that might be an owl.",
  },
  {
    n: "04",
    title: "People confirm",
    body: "Trained volunteers listen to the candidates. Only what they confirm reaches this page.",
  },
];

class HomePage extends BaseElement {
  static styles = [typography, panels];

  #state = { status: "loading", data: null, error: null };

  connectedCallback() {
    super.connectedCallback();
    // The masthead's links point at sections inside this shadow root, so it
    // asks rather than reaching in.
    this.#jump = (event) => this.#scrollTo(event.detail.section);
    window.addEventListener("bs-jump", this.#jump);
    this.#load();
  }

  disconnectedCallback() {
    window.removeEventListener("bs-jump", this.#jump);
  }

  #jump = null;

  async #load() {
    try {
      this.#state = { status: "ready", data: await api.fetchOverview(), error: null };
    } catch (error) {
      this.#state = { status: "error", data: null, error };
    }
    if (this.isConnected) this.render();
  }

  #scrollTo(section) {
    this.$(`#${section}`)?.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  render() {
    const { status, data, error } = this.#state;
    const program = data?.program ?? {};

    this.shadowRoot.innerHTML = `
      <style>
        .hero { background: var(--bs-forest); color: var(--bs-on-forest); border-bottom: 1px solid var(--bs-forest-edge); }
        .hero-inner {
          max-width: var(--bs-measure-wide);
          margin: 0 auto;
          padding: var(--bs-space-2) var(--bs-space-6) var(--bs-space-8);
          display: grid;
          grid-template-columns: minmax(0, 1.05fr) minmax(0, 0.95fr);
          gap: 3.5rem;
          align-items: center;
        }
        .hero .eyebrow { color: var(--bs-amber); margin-bottom: 1.125rem; }
        .hero h1 {
          font-size: clamp(2.125rem, 1.4rem + 2.6vw, 3.375rem);
          line-height: 1.08;
          margin-bottom: var(--bs-space-5);
          letter-spacing: -0.015em;
        }
        .hero p {
          font-size: 1.0625rem;
          line-height: 1.6;
          max-width: 46ch;
          color: var(--bs-on-forest-body);
          margin-bottom: 1.875rem;
        }
        .figures { display: flex; gap: 2.5rem; flex-wrap: wrap; }
        .figure-value { font-family: var(--bs-font-display); font-size: 2.125rem; line-height: 1; }
        .figure-label { font-size: 0.78125rem; color: var(--bs-on-forest-muted); margin-top: 0.375rem; }

        /* Stand-in for the member photo the real page carries. */
        figure {
          margin: 0;
          background: repeating-linear-gradient(135deg, #2c4a38 0 9px, #2f5039 9px 18px);
          border: 1px solid #3e6b4f;
          aspect-ratio: 4 / 3;
          display: flex;
          align-items: flex-end;
          padding: 1.125rem;
        }
        figcaption {
          font-family: var(--bs-font-mono);
          font-size: 0.71875rem;
          color: #b8c4b4;
          background: var(--bs-forest);
          padding: 0.375rem 0.5625rem;
        }

        section { max-width: var(--bs-measure-wide); margin: 0 auto; padding: 0 var(--bs-space-6); }
        #detections { padding-top: var(--bs-space-8); }
        #how { padding-top: var(--bs-space-8); padding-bottom: var(--bs-space-8); }
        .updated { font-family: var(--bs-font-mono); font-size: 0.71875rem; color: var(--bs-text-muted); }
        .caveat { font-size: 0.8125rem; color: var(--bs-text-muted); margin-top: 1.125rem; max-width: 78ch; line-height: 1.6; }

        .steps {
          display: grid;
          grid-template-columns: repeat(4, minmax(0, 1fr));
          gap: 1.75rem;
          margin-top: 2.25rem;
        }
        .step { border-top: 2px solid var(--bs-amber); padding-top: var(--bs-space-4); }
        .step .eyebrow { color: var(--bs-amber); margin-bottom: 0.625rem; }
        .step h3 { margin-bottom: var(--bs-space-2); }
        .step p { font-size: 0.875rem; line-height: 1.6; color: var(--bs-text-body); }

        .loading { padding: var(--bs-space-6) 0; color: var(--bs-text-muted); }

        @media (max-width: 900px) {
          .hero-inner { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); }
          .steps { grid-template-columns: repeat(2, minmax(0, 1fr)); }
        }
        @media (max-width: 720px) {
          .hero-inner { padding: 0 var(--bs-space-4) var(--bs-space-7); }
          figure { display: none; }
          section { padding: 0 var(--bs-space-4); }
          #detections, #how { padding-top: var(--bs-space-7); }
          .steps { grid-template-columns: minmax(0, 1fr); }
          .figures { gap: 1.375rem; }
          .figure-value { font-size: 1.5rem; }
        }
      </style>

      <div class="hero">
        <div class="hero-inner">
          <div>
            <div class="eyebrow">Acoustic monitoring · East King County</div>
            <h1>We leave recorders in the woods and listen for owls.</h1>
            <p>
              Volunteers carry SD cards home from five stations across the Eastside. Every
              night of audio runs through BirdNET, and trained volunteers confirm what the
              model heard before it appears here.
            </p>
            <div class="figures">
              ${figure(program.recorders, "recorders in the field")}
              ${figure(program.nightsRecorded, "nights recorded this year")}
              ${figure(program.confirmedDetections, "confirmed owl detections")}
            </div>
          </div>
          <figure>
            <figcaption>photo — barred owl at dusk, member submission</figcaption>
          </figure>
        </div>
      </div>

      <section id="detections" tabindex="-1">
        <div class="section-head">
          <h2>Confirmed in the last ${data?.windowDays ?? 7} nights</h2>
          ${data ? `<div class="updated">updated ${escapeHTML(stamp(data.updatedAt))}</div>` : ""}
        </div>
        ${
          status === "loading"
            ? `<p class="loading">Listening…</p>`
            : status === "error"
              ? `<p class="loading">Couldn't load the detections: ${escapeHTML(error.message)}</p>`
              : `<bs-species-table></bs-species-table>
                 <p class="caveat">
                   Every detection on this page has been listened to and confirmed by a trained
                   volunteer. BirdNET candidates that haven't been reviewed yet are not shown.
                 </p>`
        }
      </section>

      <section id="how" tabindex="-1">
        <h2>How it works</h2>
        <p class="lede" style="margin-top: var(--bs-space-2); max-width: 60ch;">
          Four steps, about six weeks from the forest floor to this page.
        </p>
        <div class="steps">
          ${STEPS.map(
            (step) => `
            <div class="step">
              <div class="eyebrow">${step.n}</div>
              <h3>${step.title}</h3>
              <p>${step.body}</p>
            </div>`,
          ).join("")}
        </div>
      </section>
    `;

    if (status === "ready") this.$("bs-species-table").species = data.species;
  }
}

function figure(value, label) {
  return `
    <div>
      <div class="figure-value">${value === undefined ? "—" : count(value)}</div>
      <div class="figure-label">${label}</div>
    </div>
  `;
}

customElements.define("bs-home-page", HomePage);
