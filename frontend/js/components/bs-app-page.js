import { BaseElement, escapeHTML } from "./base-element.js";
import { tabs, typography } from "../shared-styles.js";
import { onNavigate, path } from "../router.js";
import * as session from "../session.js";
import * as flow from "../upload-flow.js";
import "./bs-upload-steps.js";
import "./bs-upload-details.js";
import "./bs-upload-check.js";
import "./bs-upload-progress.js";
import "./bs-upload-done.js";
import "./bs-my-uploads.js";
import "./bs-detections.js";
import "./bs-detection-detail.js";
import "./bs-admin-people.js";
import "./bs-admin-recorders.js";
import "./bs-admin-uploads.js";
import "./bs-admin-upload-detail.js";

/**
 * <bs-app-page> is where everyone signed in works: one row of tabs, which are
 * routes, for volunteers and coordinators alike. A coordinator gets three more
 * of them; nothing else about the page changes with the role, so there is one
 * shell to keep working rather than two that overlap.
 *
 * Upload is the default, and holds the four steps of sending a card, from
 * /app/upload to /app/upload/done. My uploads, /app/uploads, is your own cards,
 * where an unfinished one is picked back up. Detections, /app/detections, is
 * every card's, and a detection opened from it is /app/detections/{ref}/{id},
 * keeping that list's filters in its query string.
 *
 * The coordinator's tabs keep the /admin/ paths the route guard reads, so the
 * URL still says which pages are admin-only. A card's own page,
 * /admin/uploads/{reference}, sits under All uploads, and so does each
 * detection's, /admin/uploads/{reference}/detections/{id}.
 *
 * The greeting is a salutation, not a heading: the one <h1> on every one of
 * these routes is the page's own, so a reader moving by headings lands on what
 * they came for rather than on "Good morning.".
 */
const UPLOAD = "/app/upload";
const DETECTIONS = "/app/detections";
const CARD_PREFIX = "/admin/uploads/";
const STEPS = {
  [UPLOAD]: { step: 1, tag: "bs-upload-details" },
  [`${UPLOAD}/check`]: { step: 2, tag: "bs-upload-check" },
  [`${UPLOAD}/progress`]: { step: 3, tag: "bs-upload-progress" },
  [`${UPLOAD}/done`]: { step: 4, tag: "bs-upload-done" },
};
const TABS = [
  { path: UPLOAD, label: "Upload", tag: "bs-upload-details" },
  { path: "/app/uploads", label: "My uploads", tag: "bs-my-uploads" },
  { path: DETECTIONS, label: "Detections", tag: "bs-detections" },
  { path: "/admin/uploads", label: "All uploads", tag: "bs-admin-uploads", admin: true },
  { path: "/admin/recorders", label: "Recorders", tag: "bs-admin-recorders", admin: true },
  { path: "/admin/people", label: "People", tag: "bs-admin-people", admin: true },
];

class AppPage extends BaseElement {
  static styles = [typography, tabs];

  #unsubscribe = [];

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = [
      onNavigate(() => this.render()),
      // A role arriving or changing adds or takes away the coordinator's tabs.
      session.onChange(() => this.render()),
      // The Upload tab follows the card this browser tab is sending.
      flow.subscribe(() => this.$('[data-tab="upload"]')?.setAttribute("href", uploadStep())),
    ];
  }

  disconnectedCallback() {
    for (const unsubscribe of this.#unsubscribe) unsubscribe();
  }

  render() {
    const here = path();
    const step = STEPS[here];
    const shown = TABS.filter((t) => !t.admin || session.isAdmin());
    const tab = step ? TABS[0] : (shown.find((t) => here === t.path || here.startsWith(`${t.path}/`)) ?? TABS[0]);
    // <bs-app> has no route for a path that doesn't decode, so these can't throw.
    const [listed = "", listedDetection = ""] = here.startsWith(`${DETECTIONS}/`)
      ? here.slice(DETECTIONS.length + 1).split("/").map(decodeURIComponent)
      : [];
    const [card = "", section = "", detection = ""] = here.startsWith(CARD_PREFIX)
      ? here.slice(CARD_PREFIX.length).split("/").map(decodeURIComponent)
      : [];

    this.shadowRoot.innerHTML = `
      <style>
        .head {
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: var(--bs-space-5);
          flex-wrap: wrap;
          margin-bottom: var(--bs-space-5);
        }
        /* Set like the h1 it used to be; it is a greeting, not the page's name. */
        .greeting {
          font-family: var(--bs-font-display);
          font-weight: 400;
          font-size: clamp(1.9rem, 1.3rem + 2vw, 2.375rem);
          letter-spacing: -0.01em;
        }
        /* The coordinator's tabs are the same strip, set a little apart. */
        .sep {
          flex: none;
          align-self: center;
          width: 1px;
          height: 1.125rem;
          margin: 0 var(--bs-space-3);
          background: var(--bs-border);
        }
      </style>

      <div class="head">
        <p class="greeting">${greeting()}</p>
      </div>

      <nav class="tabs" aria-label="Your sections">
        ${shown
          .map((t, i) => {
            const upload = t.path === UPLOAD;
            const sep = t.admin && !shown[i - 1]?.admin ? `<span class="sep" aria-hidden="true"></span>` : "";
            return `${sep}<a class="tab" href="${upload ? uploadStep() : t.path}" ${upload ? 'data-tab="upload"' : ""}
                       ${t === tab ? 'aria-current="page"' : ""}>${t.label}</a>`;
          })
          .join("")}
      </nav>

      ${
        step
          ? `<bs-upload-steps step="${step.step}"></bs-upload-steps><${step.tag}></${step.tag}>`
          : listed && listedDetection
            ? `<bs-detection-detail reference="${escapeHTML(listed)}" detection="${escapeHTML(listedDetection)}"
                                    list="${escapeHTML(`${DETECTIONS}${location.search}`)}"></bs-detection-detail>`
            : card && section === "detections" && detection
              ? `<bs-detection-detail reference="${escapeHTML(card)}" detection="${escapeHTML(detection)}"></bs-detection-detail>`
              : card
                ? `<bs-admin-upload-detail reference="${escapeHTML(card)}"></bs-admin-upload-detail>`
                : `<${tab.tag}></${tab.tag}>`
      }
    `;
  }
}

/**
 * Where the Upload tab goes: the step a card in this browser tab is at, so a
 * card part-way across isn't stranded behind a blank step 1, or step 1.
 */
function uploadStep() {
  const { status, files } = flow.get();
  if (files.length && (status === "uploading" || status === "paused" || status === "interrupted")) {
    return `${UPLOAD}/progress`;
  }
  if (files.length && status === "ready") return `${UPLOAD}/check`;
  return UPLOAD;
}

/** Volunteers swap cards early; the greeting should match the hour they're in. */
function greeting() {
  const hour = new Date().getHours();
  if (hour < 12) return "Good morning.";
  if (hour < 18) return "Good afternoon.";
  return "Good evening.";
}

customElements.define("bs-app-page", AppPage);
