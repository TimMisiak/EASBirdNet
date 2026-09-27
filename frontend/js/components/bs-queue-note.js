import { BaseElement, escapeHTML } from "./base-element.js";
import { panels, typography } from "../shared-styles.js";
import { queueNote } from "../upload-status.js";

/**
 * <bs-queue-note> -- why cards aren't being analyzed, on the two coordinator
 * screens that list cards sitting in Processing. Set the `queue` property with
 * the object the admin routes send alongside the cards; it renders nothing
 * while analysis is running, which is the usual case.
 *
 * It is a component rather than a few lines in each page because both pages
 * say exactly the same thing, and the wording is the point: a card that isn't
 * moving has one explanation, and a coordinator should not have to be told it
 * by someone reading container logs.
 */
class QueueNote extends BaseElement {
  static styles = [typography, panels];

  #queue = null;

  set queue(value) {
    this.#queue = value ?? null;
    if (this.isConnected) this.render();
  }

  get queue() {
    return this.#queue;
  }

  render() {
    const note = queueNote(this.#queue);
    this.shadowRoot.innerHTML = note
      ? `
      <style>
        .panel--notice { margin-bottom: var(--bs-space-5); }
        h2 { font-size: 1.0625rem; margin: 0 0 var(--bs-space-2); }
        p { margin: 0; }
        /* The server's own words: kept apart from what we're telling them,
           because it is the line to quote when asking for help. */
        .detail {
          margin-top: var(--bs-space-3);
          font-family: var(--bs-font-mono);
          font-size: 0.78125rem;
          line-height: 1.5;
          overflow-wrap: anywhere;
        }
      </style>
      <div class="panel panel--notice" role="status">
        <h2>${escapeHTML(note.headline)}</h2>
        <p>${escapeHTML(note.what)}</p>
        ${note.detail ? `<p class="detail">${escapeHTML(note.detail)}</p>` : ""}
      </div>
    `
      : "";
  }
}

customElements.define("bs-queue-note", QueueNote);
