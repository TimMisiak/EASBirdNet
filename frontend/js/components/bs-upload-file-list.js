import { escapeHTML } from "./base-element.js";
import { reset } from "../shared-styles.js";
import { fileSize } from "../format.js";
import "./bs-progress-bar.js";

/**
 * <bs-upload-file-list> -- every file on the card, each with its own bar, in
 * the order they are sent.
 *
 * Progress arrives several times a second and a card holds hundreds of files,
 * so this extends HTMLElement rather than BaseElement: rows are built once and
 * then updated in place, which keeps the list's scroll position and leaves the
 * bars that aren't moving alone. While the volunteer hasn't scrolled away, the
 * list follows the file being sent.
 *
 * Property: files -- [{path, bytes, state, sent, error}], see upload-flow.js.
 */
const LABELS = {
  waiting: "Waiting",
  done: "Uploaded",
  already: "Already uploaded",
  failed: "Not uploaded",
};

class UploadFileList extends HTMLElement {
  #list;
  #rows = new Map();
  #paths = "";
  #following = null;

  constructor() {
    super();
    this.attachShadow({ mode: "open" });
    this.shadowRoot.adoptedStyleSheets = [reset];
    this.shadowRoot.innerHTML = `${STYLE}<ol class="list" aria-label="Files on the card"></ol>`;
    this.#list = this.shadowRoot.querySelector("ol");
  }

  set files(files) {
    const paths = files.map((f) => f.path).join("\n");
    if (paths !== this.#paths) this.#build(files);
    this.#paths = paths;
    for (const file of files) this.#update(this.#rows.get(file.path), file);
    this.#follow(this.#rows.get(files.find((f) => f.state === "sending")?.path));
  }

  #build(files) {
    this.#list.innerHTML = files
      .map(
        (f) => `
        <li class="file" data-state="${escapeHTML(f.state)}">
          <span class="dot" aria-hidden="true"></span>
          <span class="name" title="${escapeHTML(f.path)}">${escapeHTML(f.path)}</span>
          <span class="size">${escapeHTML(fileSize(f.bytes))}</span>
          <bs-progress-bar size="thin" value="0" label="${escapeHTML(f.path)}"></bs-progress-bar>
          <span class="state"></span>
          <span class="why"></span>
        </li>`,
      )
      .join("");
    this.#rows = new Map(
      files.map((f, i) => {
        const li = this.#list.children[i];
        return [f.path, { li, bar: li.querySelector("bs-progress-bar"), state: li.querySelector(".state"), why: li.querySelector(".why") }];
      }),
    );
    this.#following = null;
  }

  #update(row, file) {
    const pct = file.bytes ? Math.floor((file.sent / file.bytes) * 100) : 0;
    const whole = file.state === "done" || file.state === "already";
    const value = String(whole ? 100 : pct);
    let text = LABELS[file.state] ?? "";
    if (file.state === "sending") text = `${pct}%`;
    else if (file.state === "waiting" && file.sent) text = `${pct}% sent`;

    // Only touch what changed: a bar re-renders on every attribute write.
    if (row.li.dataset.state !== file.state) row.li.dataset.state = file.state;
    if (row.bar.getAttribute("value") !== value) row.bar.setAttribute("value", value);
    if (row.state.textContent !== text) row.state.textContent = text;
    const why = file.state === "failed" ? (file.error ?? "") : "";
    if (row.why.textContent !== why) row.why.textContent = why;
  }

  /** Keep the file being sent in view, unless the volunteer has scrolled off to look at another. */
  #follow(row) {
    if (!row || row === this.#following) return;
    const watching = !this.#following || this.#inView(this.#following.li);
    this.#following = row;
    if (watching && !this.#inView(row.li)) {
      this.#list.scrollTop = row.li.offsetTop - this.#list.clientHeight / 3;
    }
  }

  #inView(li) {
    const top = li.offsetTop - this.#list.scrollTop;
    return top >= 0 && top + li.offsetHeight <= this.#list.clientHeight;
  }
}

const STYLE = `
  <style>
    :host { display: block; }
    .list {
      position: relative;
      list-style: none;
      margin: 0;
      padding: 0;
      max-height: 24rem;
      overflow-y: auto;
      border-top: 1px solid var(--bs-rule);
      border-bottom: 1px solid var(--bs-rule);
    }
    .file {
      display: grid;
      grid-template-columns: 8px minmax(0, 1fr) 4.5rem 7rem 7.5rem;
      grid-template-areas: "dot name size bar state" ". why why why why";
      align-items: center;
      column-gap: var(--bs-space-3);
      padding: var(--bs-space-2) 0;
      border-top: 1px solid var(--bs-rule);
      font-size: 0.8125rem;
    }
    .file:first-child { border-top: 0; }
    .dot { grid-area: dot; width: 8px; height: 8px; border-radius: 50%; background: var(--bs-border-strong); }
    .file[data-state="sending"] .dot { background: var(--bs-amber); }
    .file[data-state="done"] .dot,
    .file[data-state="already"] .dot { background: var(--bs-forest); }
    .file[data-state="failed"] .dot { background: var(--bs-chip-attention-text); }
    .name {
      grid-area: name;
      font-family: var(--bs-font-mono);
      font-size: 0.75rem;
      color: var(--bs-text);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .file[data-state="waiting"] .name { color: var(--bs-text-soft); }
    bs-progress-bar { grid-area: bar; }
    .size, .state {
      font-family: var(--bs-font-mono);
      font-size: 0.71875rem;
      color: var(--bs-text-muted);
      white-space: nowrap;
    }
    .size { grid-area: size; text-align: right; }
    .state { grid-area: state; }
    .file[data-state="failed"] .state { color: var(--bs-chip-attention-text); }
    .why { grid-area: why; font-size: 0.78125rem; color: var(--bs-chip-attention-text); }
    .why:empty { display: none; }
    @media (max-width: 560px) {
      .file {
        grid-template-columns: 8px minmax(0, 1fr) auto;
        grid-template-areas: "dot name state" ". bar bar" ". why why";
        row-gap: var(--bs-space-1);
      }
      .size { display: none; }
    }
  </style>
`;

customElements.define("bs-upload-file-list", UploadFileList);
