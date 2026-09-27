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
        :host { display: block; background: var(--bs-forest); color: var(--bs-on-forest); }
        .bar {
          max-width: var(--bs-measure-wide);
          margin: 0 auto;
          padding: var(--bs-space-5) var(--bs-space-6);
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: var(--bs-space-5);
        }
        nav { display: flex; align-items: center; gap: 1.625rem; font-size: 0.875rem; }
        .link {
          background: none;
          border: none;
          padding: 0;
          color: var(--bs-on-forest-nav);
          font-size: 0.875rem;
          cursor: pointer;
        }
        .link:hover { color: var(--bs-amber); }
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
          width: 20px;
          height: 1.5px;
          background: var(--bs-on-forest-nav);
        }
        .menu-button span + span { margin-top: 4px; }
        .drawer {
          display: none;
          flex-direction: column;
          gap: var(--bs-space-4);
          padding: 0 var(--bs-space-4) var(--bs-space-5);
          border-top: 1px solid var(--bs-forest-edge);
          padding-top: var(--bs-space-4);
        }
        .drawer .link { text-align: left; font-size: 1rem; }

        @media (max-width: 720px) {
          .bar { padding: var(--bs-space-4); }
          nav { display: none; }
          .menu-button { display: block; }
          .drawer[data-open="true"] { display: flex; }
        }
      </style>
      <header class="bar">
        <bs-brand-mark variant="full"></bs-brand-mark>
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
