import { BaseElement, escapeHTML } from "./base-element.js";
import { navigate } from "../router.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";

/**
 * <bs-app-header> is the signed-in masthead: who you are, and the way out.
 * It re-renders on any session change so a sign-out can't leave a stale name.
 */
class AppHeader extends BaseElement {
  #unsubscribe = null;

  connectedCallback() {
    super.connectedCallback();
    this.#unsubscribe = session.onChange(() => this.render());
  }

  disconnectedCallback() {
    this.#unsubscribe?.();
  }

  get actions() {
    return {
      signout: async () => {
        await session.signOut();
        navigate("/");
      },
      home: () => navigate("/"),
    };
  }

  render() {
    const user = session.user();
    const role = user?.role === "admin" ? "Admin" : "Volunteer";

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; background: var(--bs-forest); color: var(--bs-on-forest); }
        .bar {
          max-width: var(--bs-measure);
          margin: 0 auto;
          padding: var(--bs-space-4) var(--bs-space-6);
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: var(--bs-space-5);
          flex-wrap: wrap;
        }
        .brand { background: none; border: none; padding: 0; cursor: pointer; }
        .who { display: flex; align-items: center; gap: 1.125rem; font-size: 0.84375rem; }
        .name { color: var(--bs-on-forest-muted); }
        .signout {
          background: none;
          border: none;
          padding: 0;
          color: var(--bs-amber);
          font-size: 0.84375rem;
          cursor: pointer;
          text-decoration: underline;
          text-underline-offset: 2px;
        }
        @media (max-width: 720px) { .bar { padding: var(--bs-space-4); } }
      </style>
      <header class="bar">
        <button class="brand" data-action="home" aria-label="Birdsense home">
          <bs-brand-mark variant="compact" size="28"></bs-brand-mark>
        </button>
        <div class="who">
          <span class="name">${escapeHTML(user?.name ?? "")} · ${role}</span>
          <button class="signout" data-action="signout">Sign out</button>
        </div>
      </header>
    `;
  }
}

customElements.define("bs-app-header", AppHeader);
