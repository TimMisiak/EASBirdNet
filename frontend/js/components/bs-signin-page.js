import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { navigate } from "../router.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";

/**
 * <bs-signin-page>. The roster is the allow-list: there is no password, and
 * anyone whose address a coordinator hasn't added gets told so plainly.
 *
 * The identity providers aren't wired up yet, so the provider buttons sign in
 * against the roster directly and the boxed picker at the bottom -- which the
 * design carries as an explicit prototype affordance -- chooses which role to
 * land in. Both go away with the first real OIDC callback.
 */
class SignInPage extends BaseElement {
  static styles = [typography, controls, forms];

  #busy = false;
  #error = null;
  #remember = true;

  get actions() {
    return {
      provider: (el) => this.#signIn({ role: "volunteer", provider: el.dataset.provider }),
      role: (el) => this.#signIn({ role: el.dataset.role }),
      remember: (el) => {
        this.#remember = el.checked;
      },
    };
  }

  async #signIn(body) {
    if (this.#busy) return;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      const user = await session.signIn({ ...body, remember: this.#remember });
      navigate(user.role === "admin" ? "/admin/people" : "/app");
    } catch (error) {
      this.#busy = false;
      this.#error = error;
      this.render();
    }
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
        .remember {
          display: flex;
          align-items: center;
          gap: 0.625rem;
          margin-top: 1.375rem;
          font-size: 0.875rem;
          color: var(--bs-text-body);
          cursor: pointer;
        }
        .remember input { width: 16px; height: 16px; accent-color: var(--bs-forest); }
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
          align-items: center;
          justify-content: space-between;
          gap: var(--bs-space-3);
          flex-wrap: wrap;
        }
        .prototype .eyebrow { color: inherit; letter-spacing: 0.08em; }
        .prototype .row { gap: var(--bs-space-2); }
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
        <label class="remember">
          <input type="checkbox" data-change="remember" ${this.#remember ? "checked" : ""} />
          Remember this computer for 90 days
        </label>
        ${this.#error ? `<p class="error">${escapeHTML(this.#error.message)}</p>` : ""}
        <div class="help">
          If we don't recognize your address, email
          <a href="mailto:owls@eastsideaudubon.org">owls@eastsideaudubon.org</a>
          and a coordinator will add you.
        </div>
        <div class="prototype">
          <span class="eyebrow">Prototype — pick a role</span>
          <span class="row">
            <button class="btn btn--forest" data-action="role" data-role="volunteer" ${this.#busy ? "disabled" : ""}>Volunteer</button>
            <button class="btn btn--quiet" data-action="role" data-role="admin" ${this.#busy ? "disabled" : ""}>Admin</button>
          </span>
        </div>
      </div>
    `;
  }
}

customElements.define("bs-signin-page", SignInPage);
