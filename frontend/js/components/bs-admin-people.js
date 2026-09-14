import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { longDate } from "../format.js";
import * as api from "../api.js";
import "./bs-chip.js";

/**
 * <bs-admin-people> -- the roster, which is the whole of the access model:
 * being on this list is what lets someone sign in at all. Adding a person is
 * therefore deliberately plain, and the form says what the role means rather
 * than assuming "admin" is self-explanatory.
 */
class AdminPeople extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];

  #state = { status: "loading", people: [], error: null };
  #form = { name: "", email: "", role: "volunteer" };
  #formError = null;
  #busy = false;

  connectedCallback() {
    super.connectedCallback();
    this.#load().then(() => this.isConnected && this.render());
  }

  get actions() {
    return {
      field: (el) => {
        this.#form[el.name] = el.value;
      },
      role: (el) => {
        this.#form.role = el.value;
      },
      add: async (el, event) => {
        event.preventDefault();
        if (this.#busy) return;
        this.#busy = true;
        this.#formError = null;
        this.render();
        try {
          await api.addPerson(this.#form);
          this.#form = { name: "", email: "", role: "volunteer" };
          await this.#load();
        } catch (error) {
          this.#formError = error;
        } finally {
          this.#busy = false;
          if (this.isConnected) this.render();
        }
      },
    };
  }

  async #load() {
    try {
      const { people } = await api.fetchPeople();
      this.#state = { status: "ready", people, error: null };
    } catch (error) {
      this.#state = { status: "error", people: [], error };
    }
  }

  render() {
    const { status, people, error } = this.#state;

    this.shadowRoot.innerHTML = `
      <style>
        .columns {
          display: grid;
          grid-template-columns: minmax(0, 1.4fr) minmax(0, 0.6fr);
          gap: 2.5rem;
          align-items: start;
        }
        th { padding-top: 0; }
        td { padding-top: 0.9375rem; padding-bottom: 0.9375rem; }
        .name { font-size: 0.9375rem; }
        .email { font-size: 0.8125rem; color: var(--bs-text-muted); margin-top: 0.125rem; }
        .roles { display: flex; flex-direction: column; gap: 0.625rem; margin-bottom: var(--bs-space-5); }
        .roles label { display: flex; gap: 0.625rem; align-items: flex-start; font-size: 0.875rem; cursor: pointer; }
        .roles input { margin-top: 0.1875rem; accent-color: var(--bs-forest); }
        .roles .what { display: block; font-size: 0.78125rem; color: var(--bs-text-muted); }
        .panel p { margin-bottom: 1.125rem; }
        .error { margin-bottom: var(--bs-space-4); }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
      </style>

      <div class="columns">
        <div>
          ${
            status === "loading"
              ? `<p class="note">Loading the roster…</p>`
              : status === "error"
                ? `<p class="note">Couldn't load the roster: ${escapeHTML(error.message)}</p>`
                : `<div class="table-scroll">
                     <table>
                       <thead>
                         <tr>
                           <th scope="col">Person</th>
                           <th scope="col">Account</th>
                           <th scope="col">Role</th>
                           <th scope="col">Added</th>
                         </tr>
                       </thead>
                       <tbody>${people.map(row).join("")}</tbody>
                     </table>
                   </div>`
          }
        </div>

        <form class="panel" data-submit="add">
          <h3>Add a person</h3>
          <p class="note">They sign in with Google or Microsoft using this exact address.</p>

          <label class="label" for="person-name">Name</label>
          <input class="field field--sunk" id="person-name" name="name" type="text"
                 value="${escapeHTML(this.#form.name)}" placeholder="Alex Rivera"
                 data-change="field" style="margin-bottom: var(--bs-space-4);" />

          <label class="label" for="person-email">Email address</label>
          <input class="field field--sunk" id="person-email" name="email" type="email" required
                 value="${escapeHTML(this.#form.email)}" placeholder="name@example.com"
                 data-change="field" style="margin-bottom: var(--bs-space-4);" />

          <span class="label">Role</span>
          <div class="roles">
            <label>
              <input type="radio" name="role" value="volunteer" data-change="role"
                     ${this.#form.role === "volunteer" ? "checked" : ""} />
              <span>Volunteer<span class="what">Can upload SD cards</span></span>
            </label>
            <label>
              <input type="radio" name="role" value="admin" data-change="role"
                     ${this.#form.role === "admin" ? "checked" : ""} />
              <span>Admin<span class="what">Can also manage people and recorders</span></span>
            </label>
          </div>

          ${this.#formError ? `<p class="error">${escapeHTML(this.#formError.message)}</p>` : ""}
          <button class="btn btn--forest btn--small btn--block" ${this.#busy ? "disabled" : ""}>
            ${this.#busy ? "Adding…" : "Add to the roster"}
          </button>
        </form>
      </div>
    `;
  }
}

function row(person) {
  return `
    <tr>
      <td>
        <div class="name">${escapeHTML(person.name)}</div>
        <div class="email">${escapeHTML(person.email)}</div>
      </td>
      <td style="font-size: 0.84375rem; color: var(--bs-text-body);">${escapeHTML(person.provider)}</td>
      <td><bs-chip kind="${person.role === "admin" ? "admin" : "neutral"}">${person.role === "admin" ? "Admin" : "Volunteer"}</bs-chip></td>
      <td style="font-size: 0.84375rem; color: var(--bs-text-muted);">${escapeHTML(longDate(person.addedOn))}</td>
    </tr>
  `;
}

customElements.define("bs-admin-people", AdminPeople);
