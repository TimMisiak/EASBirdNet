import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { navigate, query } from "../router.js";
import * as api from "../api.js";
import * as session from "../session.js";
import * as roles from "../roles.js";
import "./bs-brand-mark.js";
import "./bs-password-warning.js";

/**
 * <bs-signin-page>. The roster is the allow-list: anyone whose address a
 * coordinator hasn't added gets told so plainly once a provider has proved it.
 *
 * A provider button leaves the app: it is a full navigation to
 * /api/v1/auth/{provider}/start, which redirects to the provider and comes
 * back to the callback, so nothing here is a fetch. A sign-in that failed
 * comes back to /signin?error=<reason>, which signInError turns into a
 * sentence. A sign-in the route guard sent here carries ?next=, the page they
 * were on its way to, and either kind of sign-in goes there afterwards.
 *
 * Where the server offers it, the form below them takes an address and a
 * password, and sends an emailed link for a first or forgotten one; that form
 * says nothing about who is on the roster. An admin who signs in with a password
 * is warned before going on: this page is the first that knows who they are.
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
  /** Which password form shows: the sign-in, or asking for a link. */
  #asking = false;
  /** The address a link was asked for, once it has been. */
  #asked = null;
  /** The address typed, which goes with them between those two forms. */
  #address = "";
  /** An admin just signed in with a password, held until they've read the warning. */
  #warned = null;

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

  /**
   * Where to go once signed in: the page the guard sent them here from, if it
   * is a path on this site, or the app. The server checks it again.
   */
  get #next() {
    const next = query().get("next") ?? "";
    return next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\") ? next : "/app";
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
        "session-ended": "You've been signed out. Sign in again to carry on.",
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
        const next = this.#next === "/app" ? "" : `?${new URLSearchParams({ next: this.#next })}`;
        window.location.assign(`/api/v1/auth/${encodeURIComponent(el.dataset.provider)}/start${next}`);
      },
      dev: (form) => this.#signIn({ email: form.elements.email.value }),
      password: (form) => this.#passwordSignIn(form),
      link: (form) => this.#askForLink(form),
      asking: () => this.#showForm(true),
      signing: () => this.#showForm(false),
      carryOn: () => {
        session.adopt(this.#warned);
        navigate(this.#next);
      },
    };
  }

  #showForm(asking) {
    this.#address = this.$('input[type="email"]')?.value ?? this.#address;
    this.#asking = asking;
    this.#asked = null;
    this.#error = null;
    this.render();
  }

  async #passwordSignIn(form) {
    if (this.#busy) return;
    this.#address = form.elements.email.value;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      const { user } = await api.passwordSignIn({ email: this.#address, password: form.elements.password.value });
      if (roles.has(user, roles.ADMIN)) {
        // Signed in already; the app hears of it when they carry on, because
        // hearing of it redraws the app and this page with it.
        this.#busy = false;
        this.#warned = user;
        this.render();
        return;
      }
      session.adopt(user);
      navigate(this.#next);
    } catch (error) {
      this.#busy = false;
      this.#error = error;
      this.render();
    }
  }

  async #askForLink(form) {
    if (this.#busy) return;
    this.#address = form.elements.email.value;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      await api.requestPasswordLink(this.#address);
      this.#asked = this.#address;
    } catch (error) {
      this.#error = error;
    }
    this.#busy = false;
    this.render();
  }

  async #signIn(body) {
    if (this.#busy) return;
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      await session.signIn(body);
      navigate(this.#next);
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

  /** Signing in with a password, or asking for a link to set one. */
  #passwordForm() {
    if (!session.passwordOffered()) return "";
    const busy = this.#busy ? "disabled" : "";
    const address = `
      <label class="label" for="address">Email address</label>
      <input class="field" id="address" name="email" type="email" autocomplete="username"
        value="${escapeHTML(this.#address)}" required ${busy}>
    `;
    const or = session.identityProviders().length ? `<p class="or"><span>or</span></p>` : "";
    if (this.#asked) {
      return `
        ${or}
        <p class="sent">
          If <strong>${escapeHTML(this.#asked)}</strong> is on the roster, we've emailed it a
          link to set a password. It works for an hour. Check your spam folder if you don't see it.
        </p>
        <button class="link" data-action="signing">Back to sign in</button>
      `;
    }
    if (this.#asking) {
      return `
        ${or}
        <form class="password" data-submit="link">
          <p class="sent">We'll email you a link to set a password.</p>
          ${address}
          <button class="btn btn--navy" type="submit" ${busy}>Email me a link</button>
        </form>
        <button class="link" data-action="signing">Back to sign in</button>
      `;
    }
    return `
      ${or}
      <form class="password" data-submit="password">
        ${address}
        <label class="label" for="password">Password</label>
        <input class="field" id="password" name="password" type="password" autocomplete="current-password" required ${busy}>
        <button class="btn btn--navy" type="submit" ${busy}>Sign in</button>
      </form>
      <button class="link" data-action="asking">First time, or forgot your password?</button>
    `;
  }

  /** What an admin reads after signing in with a password, before going on. */
  #warning() {
    return `
      <h1>You're signed in</h1>
      <bs-password-warning></bs-password-warning>
      <button class="btn btn--navy carry-on" data-action="carryOn">Continue</button>
    `;
  }

  #signingIn() {
    return `
      <h1>Sign in</h1>
      <p class="intro">
        Use the email address your coordinator added to the roster.
      </p>
      <div class="providers">
        ${this.#providerButtons()}
      </div>
      ${this.#passwordForm()}
      ${this.#message ? `<p class="error">${escapeHTML(this.#message)}</p>` : ""}
      <div class="help">
        If we don't recognize your address, email
        <a href="mailto:owls@eastsideaudubon.org">owls@eastsideaudubon.org</a>
        and a coordinator will add you.
      </div>
      ${this.#devPicker()}
    `;
  }

  #devPicker() {
    if (!session.isDev() || !this.#people) return "";
    const group = (label, role) => {
      const people = this.#people.filter((p) => p.role === role);
      if (!people.length) return "";
      return `
        <optgroup label="${label}">
          ${people
            .map((p) => {
              // Who does what, so each tab's guard can be tried by picking someone.
              const what = p.role === "admin" ? "" : ` — ${roles.labels(p).join(", ")}`;
              return `<option value="${escapeHTML(p.email)}">${escapeHTML(p.name + what)}</option>`;
            })
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
          <button class="btn btn--navy" type="submit" ${this.#busy ? "disabled" : ""}>Sign in</button>
        </span>
      </form>
    `;
  }

  render() {
    this.shadowRoot.innerHTML = `
      <style>
        /* The home page's barred owl, anchored right and faded into navy; the
           card sits on the navy side, lined up with the site's content edge,
           so it never covers the owl. */
        :host {
          position: relative;
          overflow: hidden;
          display: flex;
          flex: 1;
          min-height: 100vh;
          align-items: center;
          justify-content: flex-start;
          background: var(--bs-navy);
          padding: var(--bs-space-8) max(var(--bs-space-5), calc((100% - var(--bs-measure-wide)) / 2 + var(--bs-space-6)));
        }
        :host::before {
          content: "";
          position: absolute;
          inset: 0 0 0 auto;
          width: 64%;
          background: url("/images/barred-owl.jpg") 50% 22% / cover no-repeat;
          -webkit-mask-image: linear-gradient(to right, transparent, #000 38%);
          mask-image: linear-gradient(to right, transparent, #000 38%);
        }
        .card {
          position: relative;
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
          font-weight: 500;
          letter-spacing: 0;
          text-transform: none;
          gap: var(--bs-space-3);
        }
        .provider:hover:not([disabled]) { border-color: var(--bs-navy); }
        /* The providers' own logo files, unaltered, as their brand rules require. */
        .provider .logo { width: 20px; height: 20px; flex: none; }
        .or {
          display: flex;
          align-items: center;
          gap: var(--bs-space-3);
          margin-block: var(--bs-space-4);
          font-size: 0.8125rem;
          color: var(--bs-text-muted);
        }
        .or::before, .or::after { content: ""; flex: 1; border-top: 1px solid var(--bs-border); }
        .password { display: flex; flex-direction: column; }
        .password .field { margin-bottom: var(--bs-space-4); }
        .password .sent { margin-bottom: var(--bs-space-4); }
        .sent { font-size: 0.875rem; line-height: 1.6; color: var(--bs-text-body); overflow-wrap: anywhere; }
        .link {
          margin-top: var(--bs-space-3);
          padding: 0;
          border: 0;
          background: none;
          font: inherit;
          font-size: 0.8125rem;
          color: var(--bs-link);
          text-decoration: underline;
          text-underline-offset: 2px;
          cursor: pointer;
        }
        .link:hover { color: var(--bs-link-hover); }
        bs-password-warning { margin-block: var(--bs-space-4) var(--bs-space-5); }
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
          background: var(--bs-callout);
          border: 1px dashed var(--bs-callout-border);
          font-size: 0.78125rem;
          color: var(--bs-accent-edge);
          display: flex;
          flex-direction: column;
          gap: var(--bs-space-2);
        }
        .prototype .eyebrow { color: inherit; letter-spacing: 0.08em; }
        .prototype .row { display: flex; gap: var(--bs-space-2); }
        .prototype select { flex: 1; min-width: 0; font-size: 0.8125rem; color: var(--bs-text); }
        .prototype button { padding: 0.4375rem 0.75rem; font-size: 0.75rem; }
        /* CC BY requires the credit, as on the home page. */
        .credit {
          position: absolute;
          right: 0;
          bottom: 0;
          margin: 0;
          padding: 0.25rem 0.625rem;
          background: var(--bs-navy-wash);
          font-size: 0.6875rem;
          line-height: 1.4;
          color: var(--bs-on-navy-body);
        }
        .credit a { color: inherit; }
        .credit a:hover { color: var(--bs-on-navy); }

        /* Narrow, the owl moves above the card, which overlaps its faded edge. */
        @media (max-width: 900px) {
          :host {
            flex-direction: column;
            align-items: center;
            justify-content: flex-start;
            padding: 0 var(--bs-space-4) var(--bs-space-7);
          }
          :host::before {
            position: static;
            display: block;
            flex: none;
            width: calc(100% + 2 * var(--bs-space-4));
            aspect-ratio: 16 / 10;
            -webkit-mask-image: linear-gradient(to bottom, #000 70%, transparent);
            mask-image: linear-gradient(to bottom, #000 70%, transparent);
          }
          .card { margin-top: -3rem; }
          .credit { top: 0; bottom: auto; }
        }
        @media (max-width: 480px) { .card { padding: 1.75rem 1.5rem; } }
      </style>
      <div class="card">
        <bs-brand-mark variant="inline" size="48"></bs-brand-mark>
        ${this.#warned ? this.#warning() : this.#signingIn()}
      </div>
      <p class="credit">
        Barred owl, Ravenna Park, Seattle. Photo:
        <a href="https://commons.wikimedia.org/wiki/File:Barred_Owl_forest_canopy_Seattle_Washington_2026.jpg"
           target="_blank" rel="noreferrer">Guywelch2000</a>,
        <a href="https://creativecommons.org/licenses/by/4.0/" target="_blank" rel="noreferrer">CC BY 4.0</a>
      </p>
    `;
  }
}

customElements.define("bs-signin-page", SignInPage);
