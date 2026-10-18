import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, typography } from "../shared-styles.js";
import { query } from "../router.js";
import * as api from "../api.js";
import "./bs-brand-mark.js";

/**
 * <bs-connect-page> is where an LLM tool -- a claude.ai connector, say --
 * sends someone to let it read Birdsense as them (MCP; the server's
 * oauth.go). The server's authorization endpoint checks the app's request
 * and sends the browser here with it signed, as ?request=; this page says
 * which app is asking and posts Allow or Cancel, and the server answers with
 * where to send the browser: back to the app, with a code or a refusal.
 *
 * It is behind sign-in like the app, so someone who isn't signed in signs in
 * first and the guard's ?next= brings them back here. A request the server
 * couldn't hand over comes as ?error= instead.
 */
class ConnectPage extends BaseElement {
  static styles = [typography, controls, forms];

  /** {client, redirectHost, user} once loaded. */
  #request = null;
  #error = null;
  #busy = false;

  connectedCallback() {
    super.connectedCallback();
    if (this.#signed) this.#load();
  }

  get #signed() {
    return query().get("request");
  }

  async #load() {
    try {
      this.#request = await api.fetchConnectRequest(this.#signed);
    } catch (error) {
      this.#error = error.message;
    }
    this.render();
  }

  get actions() {
    return {
      allow: () => this.#decide(true),
      cancel: () => this.#decide(false),
    };
  }

  async #decide(approve) {
    if (this.#busy) return;
    this.#busy = true;
    this.render();
    try {
      const { redirect } = await api.decideConnectRequest(this.#signed, approve);
      // A full navigation, to the app that asked: it takes the code (or the
      // refusal) from there.
      window.location.assign(redirect);
    } catch (error) {
      this.#busy = false;
      this.#error = error.message;
      this.render();
    }
  }

  /** What went wrong before there was a request to show, if anything did. */
  get #failure() {
    if (this.#error) return this.#error;
    const reason = query().get("error");
    if (reason === "unknown-client") {
      return "That app isn't registered with Birdsense, or its registration is no longer valid. Remove the connection in the app and add it again.";
    }
    if (reason === "redirect-uri") {
      return "That app asked to be sent back somewhere it didn't register, so Birdsense won't send you there. Remove the connection in the app and add it again.";
    }
    if (!this.#signed) return "There's nothing to connect. Start from the app you want to connect to Birdsense.";
    return null;
  }

  #body() {
    const failure = this.#failure;
    if (failure) {
      return `
        <h1>Connect an app</h1>
        <p class="error">${escapeHTML(failure)}</p>
        <p class="help"><a href="/app">Go to Birdsense</a></p>
      `;
    }
    if (!this.#request) return `<h1>Connect an app</h1><p class="intro">Loading…</p>`;
    const { client, redirectHost, user } = this.#request;
    const busy = this.#busy ? "disabled" : "";
    return `
      <h1>Connect ${escapeHTML(client)}?</h1>
      <p class="intro">
        <strong>${escapeHTML(client)}</strong> is asking to read Birdsense as you
        (${escapeHTML(user.email)}).
      </p>
      <ul class="grants">
        <li>It will be able to see the recorders, and what BirdNET heard on them: species, times, confidence and reviews, and links to each clip.</li>
        <li>It won't be able to change anything. Reviews, cards and people stay in Birdsense.</li>
        <li>It stays connected for up to 90 days, or until you're taken off the roster.</li>
      </ul>
      <div class="actions">
        <button class="btn btn--navy" data-action="allow" ${busy}>Allow</button>
        <button class="btn btn--quiet" data-action="cancel" ${busy}>Cancel</button>
      </div>
      <p class="help">Either way, you'll go back to <strong>${escapeHTML(redirectHost)}</strong>.</p>
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
        h1 { margin-bottom: 0.625rem; overflow-wrap: anywhere; }
        .intro, .grants { font-size: 0.90625rem; line-height: 1.6; color: var(--bs-text-body); }
        .intro { margin-bottom: var(--bs-space-4); overflow-wrap: anywhere; }
        .grants { margin: 0 0 1.75rem; padding-left: 1.25rem; display: grid; gap: var(--bs-space-2); }
        .actions { display: flex; gap: var(--bs-space-3); flex-wrap: wrap; }
        .help {
          border-top: 1px solid var(--bs-border);
          margin-top: 1.625rem;
          padding-top: var(--bs-space-5);
          font-size: 0.8125rem;
          color: var(--bs-text-muted);
          line-height: 1.6;
          overflow-wrap: anywhere;
        }
        .help a { color: var(--bs-link); }
        .error { margin-top: var(--bs-space-4); }
        @media (max-width: 480px) { .card { padding: 1.75rem 1.5rem; } }
      </style>
      <div class="card">
        <bs-brand-mark variant="inline" size="48"></bs-brand-mark>
        ${this.#body()}
      </div>
    `;
  }
}

customElements.define("bs-connect-page", ConnectPage);
