import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { longDate } from "../format.js";
import * as api from "../api.js";
import * as session from "../session.js";
import "./bs-chip.js";

/**
 * <bs-admin-people> -- the roster, which is the whole of the access model:
 * being on this list is what lets someone sign in at all. Adding a person is
 * therefore deliberately plain, and the form says what the role means rather
 * than assuming "admin" is self-explanatory.
 *
 * Each row can be edited in place or removed after a confirmation. Two
 * removals are never offered: yourself, and the last admin, who would leave
 * nobody able to manage the roster. The API refuses both too; the buttons just
 * say so before anyone asks.
 */
class AdminPeople extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];

  #state = { status: "loading", people: [], error: null };
  #form = { name: "", email: "", role: "volunteer" };
  #formError = null;
  #busy = false;

  /** The row being edited, as a draft: {id, name, email, role}. */
  #editing = null;
  /** The id of the row asking "remove this person?". */
  #confirming = null;
  /** A refused or failed row action, shown under that row: {id, message}. */
  #rowError = null;
  #rowBusy = false;

  constructor() {
    super();
    this.shadowRoot.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && (this.#editing || this.#confirming)) this.actions.cancel();
    });
  }

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

      edit: (el) => {
        const person = this.#person(el.dataset.id);
        if (!person) return;
        const { id, name, email, role } = person;
        this.#editing = { id, name, email, role };
        this.#confirming = null;
        this.#rowError = null;
        this.render();
        this.$("#edit-name")?.focus();
      },
      draft: (el) => {
        this.#editing[el.name] = el.value;
      },
      draftRole: (el) => {
        // The hint under the select depends on the role, so this one re-renders.
        this.#editing.role = el.value;
        this.render();
        this.$("#edit-role")?.focus();
      },
      save: async () => {
        if (this.#rowBusy || !this.#editing) return;
        const { id, ...body } = this.#editing;
        this.#rowBusy = true;
        this.#rowError = null;
        this.render();
        try {
          await api.updatePerson(id, body);
          this.#editing = null;
          await this.#load();
          // Editing yourself changes who is signed in: the header's name, and
          // whether this page is still yours to see.
          if (id === session.user()?.id) await session.load();
        } catch (error) {
          this.#rowError = { id, message: error.message };
        } finally {
          this.#rowBusy = false;
          if (this.isConnected) this.render();
        }
      },
      cancel: () => {
        this.#editing = null;
        this.#confirming = null;
        this.#rowError = null;
        this.render();
      },

      remove: (el) => {
        const person = this.#person(el.dataset.id);
        if (!person) return;
        const blocked = this.#removalBlocked(person);
        this.#editing = null;
        this.#confirming = blocked ? null : person.id;
        this.#rowError = blocked ? { id: person.id, message: blocked } : null;
        this.render();
        // Focus the safe choice, so an Enter doesn't remove anyone.
        if (!blocked) this.$('[data-action="cancel"]')?.focus();
      },
      confirmRemove: async () => {
        const id = this.#confirming;
        if (!id || this.#rowBusy) return;
        this.#rowBusy = true;
        this.render();
        try {
          await api.removePerson(id);
          await this.#load();
        } catch (error) {
          this.#rowError = { id, message: error.message };
        } finally {
          this.#confirming = null;
          this.#rowBusy = false;
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

  #person(id) {
    return this.#state.people.find((p) => p.id === id);
  }

  #isSelf(person) {
    return person.id === session.user()?.id;
  }

  #isLastAdmin(person) {
    return person.role === "admin" && this.#state.people.filter((p) => p.role === "admin").length === 1;
  }

  /** Why this person can't be removed, or null when they can. */
  #removalBlocked(person) {
    if (this.#isSelf(person)) return "You can't remove yourself from the roster.";
    if (this.#isLastAdmin(person)) return "This is the only admin. Make someone else an admin first.";
    return null;
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
        .provider { font-size: 0.84375rem; color: var(--bs-text-body); }
        .added { font-size: 0.84375rem; color: var(--bs-text-muted); white-space: nowrap; }
        .roles { display: flex; flex-direction: column; gap: 0.625rem; margin-bottom: var(--bs-space-5); }
        .roles label { display: flex; gap: 0.625rem; align-items: flex-start; font-size: 0.875rem; cursor: pointer; }
        .roles input { margin-top: 0.1875rem; accent-color: var(--bs-forest); }
        .roles .what { display: block; font-size: 0.78125rem; color: var(--bs-text-muted); }
        .panel p { margin-bottom: 1.125rem; }
        .error { margin-bottom: var(--bs-space-4); }

        td.actions { text-align: right; white-space: nowrap; }
        td.actions .btn + .btn { margin-left: var(--bs-space-2); }

        tr.editing td { padding-top: 0.625rem; padding-bottom: 0.625rem; }
        .field--row { min-height: 2.25rem; font-size: 0.875rem; }
        #edit-email { margin-top: var(--bs-space-2); }
        #edit-role { min-width: 7.5rem; }
        .hint { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.375rem; max-width: 9rem; }

        tbody tr.sub { border-top: 0; }
        tr.sub td { padding-top: 0; }
        tr.sub .error { margin-bottom: 0; }
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
                           <th scope="col"><span class="visually-hidden">Actions</span></th>
                         </tr>
                       </thead>
                       <tbody>${people.map((person) => this.#row(person)).join("")}</tbody>
                     </table>
                   </div>
                   <form id="edit-person" data-submit="save"></form>`
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

  /** One person's row, plus the confirmation or error row under it if any. */
  #row(person) {
    const main = this.#editing?.id === person.id ? this.#editRow(person) : this.#viewRow(person);
    if (this.#confirming === person.id) return main + this.#confirmRow(person);
    if (this.#rowError?.id === person.id) {
      return `${main}
        <tr class="sub">
          <td colspan="5"><p class="error" role="alert">${escapeHTML(this.#rowError.message)}</p></td>
        </tr>`;
    }
    return main;
  }

  #viewRow(person) {
    const id = escapeHTML(person.id);
    const name = escapeHTML(person.name);
    const blocked = this.#removalBlocked(person);
    return `
      <tr>
        <td>
          <div class="name">${name}</div>
          <div class="email">${escapeHTML(person.email)}</div>
        </td>
        <td class="provider">${escapeHTML(person.provider)}</td>
        <td>${roleChip(person.role)}</td>
        <td class="added">${escapeHTML(longDate(person.addedOn))}</td>
        <td class="actions">
          <button type="button" class="btn btn--quiet btn--tiny" data-action="edit" data-id="${id}"
                  aria-label="Edit ${name}">Edit</button>
          <button type="button" class="btn btn--quiet btn--tiny btn--danger" data-action="remove" data-id="${id}"
                  aria-label="Delete ${name}"
                  ${blocked ? `aria-disabled="true" title="${escapeHTML(blocked)}"` : ""}>Delete</button>
        </td>
      </tr>
    `;
  }

  /** The inputs belong to the #edit-person form outside the table (form=). */
  #editRow(person) {
    const draft = this.#editing;
    const lastAdmin = this.#isLastAdmin(person);
    const hint = lastAdmin
      ? "The only admin has to stay an admin."
      : this.#isSelf(person) && draft.role !== "admin"
        ? "You'll lose access to this page."
        : "";
    return `
      <tr class="editing">
        <td>
          <label class="visually-hidden" for="edit-name">Name</label>
          <input class="field field--sunk field--row" id="edit-name" name="name" type="text"
                 form="edit-person" value="${escapeHTML(draft.name)}" placeholder="Name"
                 data-input="draft" />
          <label class="visually-hidden" for="edit-email">Email address</label>
          <input class="field field--sunk field--row" id="edit-email" name="email" type="email" required
                 form="edit-person" value="${escapeHTML(draft.email)}" placeholder="name@example.com"
                 data-input="draft" />
        </td>
        <td class="provider">${escapeHTML(person.provider)}</td>
        <td>
          <label class="visually-hidden" for="edit-role">Role</label>
          <select class="field field--sunk field--row" id="edit-role" name="role" form="edit-person"
                  data-change="draftRole">
            <option value="volunteer" ${draft.role === "volunteer" ? "selected" : ""} ${lastAdmin ? "disabled" : ""}>Volunteer</option>
            <option value="admin" ${draft.role === "admin" ? "selected" : ""}>Admin</option>
          </select>
          ${hint ? `<p class="hint">${hint}</p>` : ""}
        </td>
        <td class="added">${escapeHTML(longDate(person.addedOn))}</td>
        <td class="actions">
          <button class="btn btn--forest btn--tiny" form="edit-person" ${this.#rowBusy ? "disabled" : ""}>
            ${this.#rowBusy ? "Saving…" : "Save"}
          </button>
          <button type="button" class="btn btn--quiet btn--tiny" data-action="cancel">Cancel</button>
        </td>
      </tr>
    `;
  }

  #confirmRow(person) {
    return `
      <tr class="sub">
        <td colspan="5">
          <div class="confirm" role="alertdialog" aria-labelledby="confirm-text">
            <p id="confirm-text">
              Remove <strong>${escapeHTML(person.name)}</strong> from the roster?
              They won't be able to sign in. Cards they've sent are kept.
            </p>
            <span class="row">
              <button type="button" class="btn btn--tiny btn--danger-solid" data-action="confirmRemove"
                      ${this.#rowBusy ? "disabled" : ""}>${this.#rowBusy ? "Removing…" : "Remove"}</button>
              <button type="button" class="btn btn--quiet btn--tiny" data-action="cancel">Keep</button>
            </span>
          </div>
        </td>
      </tr>
    `;
  }
}

function roleChip(role) {
  return role === "admin"
    ? `<bs-chip kind="admin">Admin</bs-chip>`
    : `<bs-chip kind="neutral">Volunteer</bs-chip>`;
}

customElements.define("bs-admin-people", AdminPeople);
