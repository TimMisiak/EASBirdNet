import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { navigate } from "../router.js";
import * as api from "../api.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";

/**
 * <bs-signin-page>. The roster is the allow-list: there is no password, and
 * anyone whose address a coordinator hasn't added gets told so plainly.
 *
 * The identity providers aren't wired up yet, so the provider buttons sign in
 * as the first volunteer on the roster. In dev mode a boxed picker at the
 * bottom lists everyone on the roster to sign in as; the server only offers
 * that roster in dev mode, and the page only asks for it there.
 */
class SignInPage extends BaseElement {
  static styles = [typography, controls, forms];

  #busy = false;
  #error = null;
  /** The roster for the dev picker: null until loaded, and never outside dev. */
  #people = null;

  connectedCallback() {
    super.connectedCallback();
    if (session.isDev()) this.#loadPeople();
  }

  async #loadPeople() {
    try {
      ({ people: this.#people } = await api.fetchDevPeople());
    } catch (error) {
      this.#error = error;
    }
    this.render();
  }

  get actions() {
    return {
      provider: (el) => this.#signIn({ role: "volunteer", provider: el.dataset.provider }),
      dev: (form) => this.#signIn({ email: form.elements.email.value }),
    };
  }

  async #signIn(body) {
    if (this.#busy) return;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      const user = await session.signIn(body);
      navigate(user.role === "admin" ? "/admin/people" : "/app");
    } catch (error) {
      this.#busy = false;
      this.#error = error;
      this.render();
    }
  }

  #devPicker() {
    if (!session.isDev() || !this.#people) return "";
    const group = (label, role) => {
      const people = this.#people.filter((p) => p.role === role);
      if (!people.length) return "";
      return `
        <optgroup label="${label}">
          ${people
            .map((p) => `<option value="${escapeHTML(p.email)}">${escapeHTML(p.name)}</option>`)
            .join("")}
        </optgroup>
      `;
    };
    return `
      <form class="prototype" data-submit="dev">
        <label class="eyebrow" for="dev-person">Dev mode — sign in as</label>
        <span class="row">
          <select class="field" id="dev-person" name="email" ${this.#busy ? "disabled" : ""}>
            ${group("Admins", "admin")}
            ${group("Volunteers", "volunteer")}
          </select>
          <button class="btn btn--forest" type="submit" ${this.#busy ? "disabled" : ""}>Sign in</button>
        </span>
      </form>
    `;
  }

  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: flex;
          flex: 1;
          min-height: 100vh;
          align-items: center;
          justify-content: center;
          background: var(--bs-forest);
          padding: var(--bs-space-8) var(--bs-space-5);
        }
        .card {
          width: 440px;
          max-width: 100%;
          background: var(--bs-bg);
          border: 1px solid var(--bs-border);
          padding: 2.75rem 2.75rem 2.25rem;
        }
        bs-brand-mark { margin-bottom: 1.875rem; }
        h1 { margin-bottom: 0.625rem; }
        .intro { font-size: 0.90625rem; line-height: 1.6; color: var(--bs-text-body); margin-bottom: 1.75rem; }
        .providers { display: flex; flex-direction: column; gap: var(--bs-space-3); }
        .provider {
          min-height: 3.25rem;
          background: var(--bs-surface);
          border-color: var(--bs-border-strong);
          font-size: 0.9375rem;
          gap: var(--bs-space-3);
        }
        .provider:hover:not([disabled]) { border-color: var(--bs-forest); }
        .provider .glyph {
          width: 20px; height: 20px;
          border-radius: 50%;
          background: var(--bs-bg-deep);
          display: inline-flex;
          align-items: center;
          justify-content: center;
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
        }
        .provider .glyph[data-square="true"] { border-radius: 0; }
        .help {
          border-top: 1px solid var(--bs-border);
          margin-top: 1.625rem;
          padding-top: var(--bs-space-5);
          font-size: 0.8125rem;
          color: var(--bs-text-muted);
          line-height: 1.6;
        }
        .error { margin-top: var(--bs-space-4); }
        .prototype {
          margin-top: 1.375rem;
          padding: var(--bs-space-3) 0.875rem;
          background: var(--bs-parchment);
          border: 1px dashed var(--bs-notice-border);
          font-size: 0.78125rem;
          color: var(--bs-amber-edge);
          display: flex;
          flex-direction: column;
          gap: var(--bs-space-2);
        }
        .prototype .eyebrow { color: inherit; letter-spacing: 0.08em; }
        .prototype .row { display: flex; gap: var(--bs-space-2); }
        .prototype select { flex: 1; min-width: 0; font-size: 0.8125rem; color: var(--bs-text); }
        .prototype button { padding: 0.4375rem 0.75rem; font-size: 0.75rem; }
        @media (max-width: 480px) { .card { padding: 1.75rem 1.5rem; } }
      </style>
      <div class="card">
        <bs-brand-mark variant="inline" on="light" size="30"></bs-brand-mark>
        <h1>Sign in</h1>
        <p class="intro">
          Use the email address your coordinator added to the roster. We don't keep a
          password for you.
        </p>
        <div class="providers">
          <button class="btn provider" data-action="provider" data-provider="Google" ${this.#busy ? "disabled" : ""}>
            <span class="glyph" aria-hidden="true">G</span> Continue with Google
          </button>
          <button class="btn provider" data-action="provider" data-provider="Microsoft" ${this.#busy ? "disabled" : ""}>
            <span class="glyph" data-square="true" aria-hidden="true">M</span> Continue with Microsoft
          </button>
        </div>
        ${this.#error ? `<p class="error">${escapeHTML(this.#error.message)}</p>` : ""}
        <div class="help">
          If we don't recognize your address, email
          <a href="mailto:owls@eastsideaudubon.org">owls@eastsideaudubon.org</a>
          and a coordinator will add you.
        </div>
        ${this.#devPicker()}
      </div>
    `;
  }
}

customElements.define("bs-signin-page", SignInPage);
