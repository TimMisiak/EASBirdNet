import { BaseElement } from "./base-element.js";
import { controls, typography } from "../shared-styles.js";
import { navigate, onNavigate, path } from "../router.js";
import * as flow from "../upload-flow.js";
import "./bs-admin-people.js";
import "./bs-admin-recorders.js";
import "./bs-admin-uploads.js";

/**
 * <bs-admin-page> is the coordinator's shell. The three tabs are routes, not
 * local state, so a coordinator can link a colleague straight to the uploads
 * table -- and so the back button works the way it looks like it should.
 */
const TABS = [
  { path: "/admin/people", label: "People", tag: "bs-admin-people" },
  { path: "/admin/recorders", label: "Recorders", tag: "bs-admin-recorders" },
  { path: "/admin/uploads", label: "All uploads", tag: "bs-admin-uploads" },
];

class AdminPage extends BaseElement {
  static styles = [typography, controls];

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
    const tab = TABS.find((t) => t.path === here) ?? TABS[0];

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
        .tabs {
          display: flex;
          gap: var(--bs-space-1);
          border-bottom: 1px solid var(--bs-border);
          margin-bottom: 2.125rem;
          overflow-x: auto;
        }
        .tab {
          border-bottom: 2px solid transparent;
          color: var(--bs-text-muted);
          padding: 0.625rem var(--bs-space-4);
          margin-bottom: -1px;
          font-size: 0.90625rem;
          text-decoration: none;
          white-space: nowrap;
        }
        .tab:hover { color: var(--bs-text); }
        .tab[aria-current="page"] { border-bottom-color: var(--bs-amber); color: var(--bs-text); }
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

      <${tab.tag}></${tab.tag}>
    `;
  }
}

customElements.define("bs-admin-page", AdminPage);
