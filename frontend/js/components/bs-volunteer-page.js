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

/**
 * <bs-volunteer-page> is where a signed-in volunteer works, laid out like the
 * coordinator's <bs-admin-page>: a row of tabs that are routes.
 *
 * Upload is the default, and holds the four steps of sending a card, from
 * /app/upload to /app/upload/done. My uploads, /app/uploads, is the volunteer's
 * own cards, where an unfinished one is picked back up. Detections is the same
 * list a coordinator has; a detection opened from it is
 * /app/detections/{reference}/{id}, and keeps the list's filters in its query
 * string.
 */
const UPLOAD = "/app/upload";
const DETECTIONS = "/app/detections";
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
];

class VolunteerPage extends BaseElement {
  static styles = [typography, tabs];

  #unsubscribe = [];

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = [
      onNavigate(() => this.render()),
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
    const tab = step ? TABS[0] : (TABS.find((t) => here === t.path || here.startsWith(`${t.path}/`)) ?? TABS[0]);
    const [reference = "", detection = ""] = here.startsWith(`${DETECTIONS}/`)
      ? here.slice(DETECTIONS.length + 1).split("/").map(decodeURIComponent)
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
      </style>

      <div class="head">
        <h1>${greeting()}</h1>
        ${session.isAdmin() ? `<a href="/admin/people">Program admin →</a>` : ""}
      </div>

      <nav class="tabs" aria-label="Your sections">
        ${TABS.map((t) => {
          const upload = t.path === UPLOAD;
          return `<a class="tab" href="${upload ? uploadStep() : t.path}" ${upload ? 'data-tab="upload"' : ""}
                     ${t === tab ? 'aria-current="page"' : ""}>${t.label}</a>`;
        }).join("")}
      </nav>

      ${
        step
          ? `<bs-upload-steps step="${step.step}"></bs-upload-steps><${step.tag}></${step.tag}>`
          : reference && detection
            ? `<bs-detection-detail reference="${escapeHTML(reference)}" detection="${escapeHTML(detection)}"
                                    list="${escapeHTML(`${DETECTIONS}${location.search}`)}"></bs-detection-detail>`
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

customElements.define("bs-volunteer-page", VolunteerPage);
