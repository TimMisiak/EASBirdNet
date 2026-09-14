import { BaseElement } from "./base-element.js";

/** <bs-site-footer> -- the dark rule at the bottom of the public page. */
class SiteFooter extends BaseElement {
  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: block;
          background: var(--bs-text);
          color: var(--bs-on-forest-muted);
          font-size: 0.8125rem;
        }
        .inner {
          max-width: var(--bs-measure-wide);
          margin: 0 auto;
          padding: var(--bs-space-6);
          display: flex;
          justify-content: space-between;
          gap: var(--bs-space-5);
          flex-wrap: wrap;
        }
        a { color: var(--bs-amber); text-decoration: none; }
        a:hover { text-decoration: underline; text-underline-offset: 2px; }
        .links { display: flex; gap: 1.375rem; }
      </style>
      <footer class="inner">
        <div>Eastside Audubon Society · P.O. Box 3113, Redmond, WA 98073</div>
        <div class="links">
          <a href="https://www.eastsideaudubon.org/" target="_blank" rel="noreferrer">eastsideaudubon.org</a>
          <a href="https://www.eastsideaudubon.org/contact" target="_blank" rel="noreferrer">Contact</a>
        </div>
      </footer>
    `;
  }
}

customElements.define("bs-site-footer", SiteFooter);
