import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { navigate, query } from "../router.js";
import * as api from "../api.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";

/**
 * <bs-signin-page>. The roster is the allow-list: there is no password, and
 * anyone whose address a coordinator hasn't added gets told so plainly.
 *
 * A provider button leaves the app: it is a full navigation to
 * /api/v1/auth/{provider}/start, which redirects to the provider and comes
 * back to the callback, so nothing here is a fetch. A sign-in that failed
 * comes back to /signin?error=<reason>, which signInError turns into a
 * sentence.
 *
 * In dev mode a boxed picker at the bottom lists everyone on the roster to
 * sign in as; the server only offers that roster in dev mode, and the page
 * only asks for it there.
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

  /** What the callback said when it sent the browser back here. */
  get #failure() {
    const reason = query().get("error");
    if (!reason) return null;
    return (
      {
        "not-on-roster":
          "That address isn't on the roster yet. Ask a coordinator to add it, then try again.",
        "unverified-email":
          "Your provider didn't confirm that address is yours, so we can't match it to the roster.",
        denied: "Sign-in was cancelled.",
        expired: "That took a while — please try again.",
        state: "That sign-in didn't look right. Please try again.",
        "provider-unreachable": "We couldn't reach the sign-in provider. Please try again in a moment.",
      }[reason] ?? "Sign-in didn't work. Please try again."
    );
  }

  get actions() {
    return {
      provider: (el) => {
        this.#busy = true;
        this.render();
        // A full navigation, not a fetch: the provider will redirect the
        // browser back to the callback.
        window.location.assign(`/api/v1/auth/${encodeURIComponent(el.dataset.provider)}/start`);
      },
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
      navigate("/app");
    } catch (error) {
      this.#busy = false;
      this.#error = error;
      this.render();
    }
  }

  /** The one thing to tell them, whether it came from the callback or a fetch. */
  get #message() {
    return this.#error?.message ?? this.#failure;
  }

  /**
   * One button per provider the server is configured for, so a button never
   * leads to a route that isn't registered.
   */
  #providerButtons() {
    const known = {
      microsoft: { label: "Continue with Microsoft", logo: "/images/microsoft-logo.svg" },
      google: { label: "Continue with Google", logo: "/images/google-g.svg" },
    };
    const offered = session.identityProviders().filter((name) => known[name]);
    if (!offered.length) {
      return session.isDev()
        ? ""
        : `<p class="error">Sign-in isn't configured on this server yet.</p>`;
    }
    return offered
      .map((name) => {
        const { label, logo } = known[name];
        return `
          <button class="btn provider" data-action="provider" data-provider="${escapeHTML(name)}" ${this.#busy ? "disabled" : ""}>
            <img class="logo" src="${escapeHTML(logo)}" alt="" width="20" height="20"> ${escapeHTML(label)}
          </button>
        `;
      })
      .join("");
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
        /* The providers' own logo files, unaltered, as their brand rules require. */
        .provider .logo { width: 20px; height: 20px; flex: none; }
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
          Use the email address your coordinator added to the roster.
        </p>
        <div class="providers">
          ${this.#providerButtons()}
        </div>
        ${this.#message ? `<p class="error">${escapeHTML(this.#message)}</p>` : ""}
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
