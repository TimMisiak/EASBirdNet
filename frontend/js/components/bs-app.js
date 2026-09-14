import { BaseElement } from "./base-element.js";

/**
 * <bs-app> is the page shell: masthead, main content, footer. It owns layout
 * only -- data fetching belongs to the components that display the data.
 */
class BirdsenseApp extends BaseElement {
  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: block;
          max-width: var(--bs-measure);
          margin: 0 auto;
          padding: var(--bs-space-4) var(--bs-space-3) var(--bs-space-5);
        }
        header {
          border-bottom: 1px solid var(--bs-border);
          padding-bottom: var(--bs-space-3);
          margin-bottom: var(--bs-space-4);
        }
        h1 {
          margin: 0;
          font-size: 1.6rem;
          letter-spacing: -0.01em;
        }
        p {
          margin: var(--bs-space-1) 0 0;
          color: var(--bs-text-muted);
        }
        footer {
          margin-top: var(--bs-space-5);
          padding-top: var(--bs-space-3);
          border-top: 1px solid var(--bs-border);
          color: var(--bs-text-muted);
          font-size: 0.85rem;
        }
      </style>
      <header>
        <h1>Birdsense</h1>
        <p>Bird calls heard at Eastside Audubon listening stations.</p>
      </header>
      <main>
        <bs-detection-list></bs-detection-list>
      </main>
      <footer>Eastside Audubon Society</footer>
    `;
  }
}

customElements.define("bs-app", BirdsenseApp);
