import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, tabs, typography } from "../shared-styles.js";
import { navigate, onNavigate, path } from "../router.js";
import * as flow from "../upload-flow.js";
import "./bs-admin-people.js";
import "./bs-admin-recorders.js";
import "./bs-admin-uploads.js";
import "./bs-admin-upload-detail.js";
import "./bs-detections.js";
import "./bs-detection-detail.js";

/**
 * <bs-admin-page> is the coordinator's shell. The tabs are routes, not local
 * state, so a coordinator can link a colleague straight to the uploads table --
 * and so the back button works the way it looks like it should. A card's own
 * page, /admin/uploads/{reference}, sits under the uploads tab, and so does each
 * detection's, /admin/uploads/{reference}/detections/{id}. The same detection
 * opened from the detections tab is /admin/detections/{reference}/{id}, and
 * keeps that list's filters in its query string.
 */
const CARD_PREFIX = "/admin/uploads/";
const DETECTION_PREFIX = "/admin/detections/";
const TABS = [
  { path: "/admin/people", label: "People", tag: "bs-admin-people" },
  { path: "/admin/recorders", label: "Recorders", tag: "bs-admin-recorders" },
  { path: "/admin/uploads", label: "All uploads", tag: "bs-admin-uploads" },
  { path: "/admin/detections", label: "Detections", tag: "bs-detections" },
];

class AdminPage extends BaseElement {
  static styles = [typography, controls, tabs];

  connectedCallback() {
    super.connectedCallback();
    onNavigate(() => this.render());
  }

  get actions() {
    return {
      upload: () => {
        flow.reset();
        navigate("/app/upload");
      },
    };
  }

  render() {
    const here = path();
    const tab = TABS.find((t) => here === t.path || here.startsWith(`${t.path}/`)) ?? TABS[0];
    const segments = (prefix) =>
      here.startsWith(prefix) ? here.slice(prefix.length).split("/").map(decodeURIComponent) : [];
    const [card = "", section = "", detection = ""] = segments(CARD_PREFIX);
    const [listed = "", listedDetection = ""] = segments(DETECTION_PREFIX);

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
        .head .row { align-items: center; gap: var(--bs-space-4); }
      </style>

      <div class="head">
        <h1>Program admin</h1>
        <span class="row">
          <span class="note">You can collect cards too.</span>
          <button class="btn btn--forest btn--small" data-action="upload">Upload an SD card →</button>
        </span>
      </div>

      <nav class="tabs" aria-label="Admin sections">
        ${TABS.map(
          (t) =>
            `<a class="tab" href="${t.path}" ${t.path === tab.path ? 'aria-current="page"' : ""}>${t.label}</a>`,
        ).join("")}
      </nav>

      ${
        listed && listedDetection
          ? `<bs-detection-detail reference="${escapeHTML(listed)}" detection="${escapeHTML(listedDetection)}"
                                  list="${escapeHTML(`/admin/detections${location.search}`)}"></bs-detection-detail>`
          : card && section === "detections" && detection
          ? `<bs-detection-detail reference="${escapeHTML(card)}" detection="${escapeHTML(detection)}"></bs-detection-detail>`
          : card
          ? `<bs-admin-upload-detail reference="${escapeHTML(card)}"></bs-admin-upload-detail>`
          : `<${tab.tag}></${tab.tag}>`
      }
    `;
  }
}

customElements.define("bs-admin-page", AdminPage);
