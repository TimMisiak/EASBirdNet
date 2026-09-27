import { BaseElement } from "./base-element.js";
import { controls } from "../shared-styles.js";
import { navigate } from "../router.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";

/**
 * <bs-site-header> is the public masthead. Its in-page links can't use a plain
 * fragment -- the sections they point at live inside another component's shadow
 * root -- so it raises a composed `bs-jump` event and the page scrolls itself.
 */
class SiteHeader extends BaseElement {
  static styles = [controls];

  #open = false;

  get actions() {
    return {
      jump: (el) => {
        this.#open = false;
        this.dispatchEvent(
          new CustomEvent("bs-jump", {
            detail: { section: el.dataset.section },
            bubbles: true,
            composed: true,
          }),
        );
        this.render();
      },
      signin: () => {
        if (!session.isSignedIn()) return navigate("/signin");
        navigate("/app");
      },
      toggle: () => {
        this.#open = !this.#open;
        this.render();
      },
    };
  }

  render() {
    const cta = !session.isSignedIn() ? "Sign in" : session.isAdmin() ? "Admin" : "Upload";

    this.shadowRoot.innerHTML = `
      <style>
        /* The main site's masthead: white, the logo on the left, the nav in
           bold uppercase on the right. */
        :host {
          display: block;
          background: var(--bs-surface);
          color: var(--bs-text);
          border-bottom: 1px solid var(--bs-rule);
        }
        .bar {
          max-width: var(--bs-measure-wide);
          margin: 0 auto;
          padding: var(--bs-space-4) var(--bs-space-6);
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: var(--bs-space-5);
        }
        .home { display: block; color: inherit; text-decoration: none; }
        nav { display: flex; align-items: center; gap: 1.75rem; }
        .link {
          background: none;
          border: none;
          padding: 0;
          color: var(--bs-text);
          font-size: 0.875rem;
          font-weight: 700;
          letter-spacing: 0.07em;
          text-transform: uppercase;
          cursor: pointer;
        }
        .link:hover { color: var(--bs-accent); }
        .menu-button {
          display: none;
          background: none;
          border: none;
          padding: var(--bs-space-2);
          margin: calc(var(--bs-space-2) * -1);
          cursor: pointer;
        }
        .menu-button span {
          display: block;
          width: 22px;
          height: 2px;
          background: var(--bs-text);
        }
        .menu-button span + span { margin-top: 5px; }
        .drawer {
          display: none;
          flex-direction: column;
          align-items: flex-start;
          gap: var(--bs-space-4);
          padding: var(--bs-space-4) var(--bs-space-4) var(--bs-space-5);
          border-top: 1px solid var(--bs-rule);
        }
        .drawer .link { text-align: left; font-size: 0.9375rem; }

        @media (max-width: 720px) {
          .bar { padding: var(--bs-space-3) var(--bs-space-4); }
          nav { display: none; }
          .menu-button { display: block; }
          .drawer[data-open="true"] { display: flex; }
        }
      </style>
      <header class="bar">
        <a class="home" href="/" aria-label="Birdsense home">
          <bs-brand-mark variant="full" size="64"></bs-brand-mark>
        </a>
        <nav aria-label="Site">
          <button class="link" data-action="jump" data-section="detections">Detections</button>
          <button class="link" data-action="jump" data-section="how">How it works</button>
          <button class="btn btn--primary btn--small" data-action="signin">
            ${cta}
          </button>
        </nav>
        <button class="menu-button" data-action="toggle"
                aria-expanded="${this.#open}" aria-label="Menu">
          <span></span><span></span><span></span>
        </button>
      </header>
      <div class="drawer" data-open="${this.#open}">
        <button class="link" data-action="jump" data-section="detections">Detections</button>
        <button class="link" data-action="jump" data-section="how">How it works</button>
        <button class="btn btn--primary btn--small" data-action="signin">
          ${cta}
        </button>
      </div>
    `;
  }
}

customElements.define("bs-site-header", SiteHeader);
