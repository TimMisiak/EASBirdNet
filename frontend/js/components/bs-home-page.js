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
        /* The banner: a barred owl -- the bird the program listens for --
           anchored right and faded into navy, with the text on the navy side
           so it never sits over the owl. */
        .hero {
          position: relative;
          overflow: hidden;
          background: var(--bs-navy);
          color: var(--bs-on-navy);
        }
        .hero::before {
          content: "";
          position: absolute;
          inset: 0 0 0 auto;
          width: 64%;
          background: url("/images/barred-owl.jpg") 50% 22% / cover no-repeat;
          -webkit-mask-image: linear-gradient(to right, transparent, #000 38%);
          mask-image: linear-gradient(to right, transparent, #000 38%);
        }
        .hero-inner {
          max-width: var(--bs-measure-wide);
          margin: 0 auto;
          padding: 5.5rem var(--bs-space-6) 6rem;
        }
        /* Positioned so it paints above the photo layer. */
        .hero-copy { position: relative; max-width: 34rem; }
        .hero .eyebrow {
          font-family: var(--bs-font);
          font-weight: 700;
          color: var(--bs-on-navy);
          margin-bottom: 1.125rem;
        }
        .hero h1 {
          color: var(--bs-on-navy);
          font-weight: 700;
          font-size: clamp(2.125rem, 1.4rem + 2.6vw, 3.375rem);
          line-height: 1.1;
          margin-bottom: var(--bs-space-5);
        }
        .hero p {
          font-size: 1.125rem;
          line-height: 1.6;
          max-width: 46ch;
          margin: 0 0 2.25rem;
          color: var(--bs-on-navy-body);
        }
        .figures { display: flex; gap: 3rem; flex-wrap: wrap; }
        .figure-value { font-family: var(--bs-font-display); font-weight: 700; font-size: 2.25rem; line-height: 1; }
        .figure-label { font-size: 0.8125rem; color: var(--bs-on-navy-muted); margin-top: 0.375rem; }
        /* CC BY requires the credit; it sits in the photo's corner, on its own
           backing so it reads over bright leaves. */
        .hero .credit {
          position: absolute;
          right: 0;
          bottom: 0;
          margin: 0;
          max-width: none;
          padding: 0.25rem 0.625rem;
          background: var(--bs-navy-wash);
          font-size: 0.6875rem;
          line-height: 1.4;
          color: var(--bs-on-navy-body);
        }
        .credit a { color: inherit; }
        .credit a:hover { color: var(--bs-on-navy); }

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
        .step { border-top: 3px solid var(--bs-accent); padding-top: var(--bs-space-4); }
        .step .eyebrow { color: var(--bs-accent-ink); margin-bottom: 0.625rem; }
        .step h3 { margin-bottom: var(--bs-space-2); }
        .step p { font-size: 0.875rem; line-height: 1.6; color: var(--bs-text-body); }

        .loading { padding: var(--bs-space-6) 0; color: var(--bs-text-muted); }

        /* Narrow, the owl moves above the text rather than behind it. */
        @media (max-width: 900px) {
          .hero::before {
            position: static;
            display: block;
            width: 100%;
            aspect-ratio: 16 / 10;
            -webkit-mask-image: linear-gradient(to bottom, #000 70%, transparent);
            mask-image: linear-gradient(to bottom, #000 70%, transparent);
          }
          .hero-inner { padding-top: var(--bs-space-5); }
          .hero .credit { top: 0; bottom: auto; }
          .steps { grid-template-columns: repeat(2, minmax(0, 1fr)); }
        }
        @media (max-width: 720px) {
          .hero-inner { padding: var(--bs-space-5) var(--bs-space-4) var(--bs-space-8); }
          section { padding: 0 var(--bs-space-4); }
          #detections, #how { padding-top: var(--bs-space-7); }
          .steps { grid-template-columns: minmax(0, 1fr); }
          .figures { gap: 1.375rem; }
          .figure-value { font-size: 1.5rem; }
        }
      </style>

      <div class="hero">
        <div class="hero-inner">
          <div class="hero-copy">
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
          <p class="credit">
            Barred owl, Ravenna Park, Seattle. Photo:
            <a href="https://commons.wikimedia.org/wiki/File:Barred_Owl_forest_canopy_Seattle_Washington_2026.jpg"
               target="_blank" rel="noreferrer">Guywelch2000</a>,
            <a href="https://creativecommons.org/licenses/by/4.0/" target="_blank" rel="noreferrer">CC BY 4.0</a>
          </p>
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
