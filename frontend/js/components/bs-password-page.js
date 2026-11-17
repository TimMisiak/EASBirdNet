import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { navigate, query } from "../router.js";
import * as api from "../api.js";
import * as roles from "../roles.js";
import * as session from "../session.js";
import "./bs-brand-mark.js";
import "./bs-password-warning.js";

/** The server's rule, here so the form can say so before anything is sent. */
const MIN_LENGTH = 12;

/**
 * <bs-password-page> is where an emailed link lands, to set a first password
 * or replace a forgotten one (the server's password.go). The link carries its
 * token as ?token=; opening this page spends nothing, so a mail scanner that
 * follows the link does no harm, and only submitting the form uses it.
 *
 * Setting a password signs out everything else of theirs and signs this
 * browser in. An admin is warned before they choose one.
 */
class PasswordPage extends BaseElement {
  static styles = [typography, controls, forms];

  /** {user, first} once the link has been read. */
  #link = null;
  #failure = null;
  #error = null;
  #busy = false;

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  get #token() {
    return query().get("token") ?? "";
  }

  async #load() {
    try {
      this.#link = await api.fetchPasswordLink(this.#token);
    } catch (error) {
      this.#failure = error.message;
    }
    this.render();
  }

  get actions() {
    return {
      save: (form) => this.#save(form),
    };
  }

  async #save(form) {
    if (this.#busy) return;
    const password = form.elements.password.value;
    if ([...password].length < MIN_LENGTH) {
      this.#error = `Please choose a password of at least ${MIN_LENGTH} characters.`;
      this.render();
      return;
    }
    this.#busy = true;
    this.#error = null;
    this.render();
    try {
      const { user } = await api.setPassword({ token: this.#token, password });
      session.adopt(user);
      navigate("/app");
    } catch (error) {
      this.#busy = false;
      this.#error = error.message;
      this.render();
    }
  }

  #body() {
    if (this.#failure) {
      return `
        <h1>Choose a password</h1>
        <p class="error">${escapeHTML(this.#failure)}</p>
        <p class="help"><a href="/signin">Back to sign in</a></p>
      `;
    }
    if (!this.#link) return `<h1>Choose a password</h1><p class="intro">Loading…</p>`;
    const { user, first } = this.#link;
    const busy = this.#busy ? "disabled" : "";
    return `
      <h1>${first ? "Choose a password" : "Choose a new password"}</h1>
      <p class="intro">
        For <strong>${escapeHTML(user.email)}</strong>.
        ${first ? "" : "This signs you out everywhere else, including any connected apps."}
      </p>
      ${roles.has(user, roles.ADMIN) ? "<bs-password-warning></bs-password-warning>" : ""}
      <form data-submit="save">
        <input type="email" name="username" value="${escapeHTML(user.email)}" autocomplete="username" hidden>
        <label class="label" for="password">Password</label>
        <input class="field" id="password" name="password" type="password" autocomplete="new-password"
          minlength="${MIN_LENGTH}" maxlength="256" required ${busy}>
        <p class="hint">At least ${MIN_LENGTH} characters. A few unrelated words work well.</p>
        ${this.#error ? `<p class="error">${escapeHTML(this.#error)}</p>` : ""}
        <button class="btn btn--navy" type="submit" ${busy}>Save password</button>
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
          background: var(--bs-navy);
          padding: var(--bs-space-8) var(--bs-space-4);
        }
        .card {
          width: 480px;
          max-width: 100%;
          background: var(--bs-bg);
          border: 1px solid var(--bs-border);
          padding: 2.75rem 2.75rem 2.25rem;
        }
        bs-brand-mark { margin-bottom: 1.875rem; }
        h1 { margin-bottom: 0.625rem; }
        .intro { font-size: 0.90625rem; line-height: 1.6; color: var(--bs-text-body); margin-bottom: var(--bs-space-4); overflow-wrap: anywhere; }
        bs-password-warning { margin-bottom: var(--bs-space-5); }
        form { display: flex; flex-direction: column; }
        .hint { font-size: 0.8125rem; color: var(--bs-text-muted); margin-block: var(--bs-space-2) var(--bs-space-4); }
        .help {
          border-top: 1px solid var(--bs-border);
          margin-top: 1.625rem;
          padding-top: var(--bs-space-5);
          font-size: 0.8125rem;
          color: var(--bs-text-muted);
        }
        .help a { color: var(--bs-link); }
        .error { margin-block: 0 var(--bs-space-4); }
        @media (max-width: 480px) { .card { padding: 1.75rem 1.5rem; } }
      </style>
      <div class="card">
        <bs-brand-mark variant="inline" size="48"></bs-brand-mark>
        ${this.#body()}
      </div>
    `;
  }
}

customElements.define("bs-password-page", PasswordPage);
