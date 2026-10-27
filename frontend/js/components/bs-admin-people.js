import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, forms, panels, tables, typography } from "../shared-styles.js";
import { longDate } from "../format.js";
import * as api from "../api.js";
import * as session from "../session.js";
import { ADMIN, BIRDING_EXPERT, FIELD_VOLUNTEER, REVIEWER, VOLUNTEER_ROLES, labels } from "../roles.js";
import "./bs-chip.js";

/**
 * <bs-admin-people> -- the roster, which is the whole of the access model:
 * being on this list is what lets someone sign in at all, and their roles are
 * what they reach once they have (roles.js). Adding a person is therefore
 * deliberately plain, and the form says what each role is for rather than
 * assuming the names are self-explanatory.
 *
 * Roles are toggles, any number of them: the three volunteer roles, and Admin,
 * which is everything. An admin keeps the volunteer roles they're given, for if
 * they stop being one. A volunteer needs at least one, which the form holds to
 * before the API does. Birding expert includes Reviewer, so ticking it shows
 * Reviewer ticked too, and fixed.
 *
 * Each row can be edited in place or removed after a confirmation. Two
 * removals are never offered: yourself, and the last admin, who would leave
 * nobody able to manage the roster. The API refuses both too; the buttons just
 * say so before anyone asks.
 */
class AdminPeople extends BaseElement {
  static styles = [typography, controls, forms, panels, tables];

  #state = { status: "loading", people: [], error: null };
  #form = blankForm();
  #formError = null;
  #busy = false;

  /** The row being edited, as a draft: {id, name, email, admin, roles}. */
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
      // A role ticked or cleared in either form. Birding expert decides
      // whether Reviewer can be, and the hints depend on all of them, so it
      // re-renders and puts the focus back where it was.
      role: (el) => {
        toggle(el.dataset.draft ? this.#editing : this.#form, el);
        this.render();
        this.$(`#${el.id}`)?.focus();
      },
      add: async (el, event) => {
        event.preventDefault();
        if (this.#busy || !hasAccess(this.#form)) return;
        this.#busy = true;
        this.#formError = null;
        this.render();
        try {
          await api.addPerson(bodyOf(this.#form));
          this.#form = blankForm();
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
        const { id, name, email, role, roles = [] } = person;
        this.#editing = { id, name, email, admin: role === ADMIN, roles: [...roles] };
        this.#confirming = null;
        this.#rowError = null;
        this.render();
        this.$("#edit-name")?.focus();
      },
      draft: (el) => {
        this.#editing[el.name] = el.value;
      },
      save: async () => {
        if (this.#rowBusy || !this.#editing || !hasAccess(this.#editing)) return;
        const { id, ...draft } = this.#editing;
        const body = bodyOf(draft);
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
        .roles input { margin-top: 0.1875rem; accent-color: var(--bs-navy); }
        .roles input:disabled + span { color: var(--bs-text-muted); }
        .roles label:has(input:disabled) { cursor: default; }
        .roles .what { display: block; font-size: 0.78125rem; color: var(--bs-text-muted); }
        .roles--row { gap: 0.375rem; margin-bottom: 0; }
        .roles--row label { font-size: 0.8125rem; white-space: nowrap; }
        .roles .admin { padding-top: 0.5rem; border-top: 1px solid var(--bs-rule); }
        .roles--row .admin { padding-top: 0.375rem; }
        .chips { display: flex; flex-wrap: wrap; gap: 0.375rem; }
        fieldset.bare { border: 0; margin: 0; padding: 0; min-width: 0; }
        legend.label { padding: 0; }
        .panel p { margin-bottom: 1.125rem; }
        .error { margin-bottom: var(--bs-space-4); }

        td.actions { text-align: right; white-space: nowrap; }
        td.actions .btn + .btn { margin-left: var(--bs-space-2); }

        tr.editing td { padding-top: 0.625rem; padding-bottom: 0.625rem; }
        .field--row { min-height: 2.25rem; font-size: 0.875rem; }
        #edit-email { margin-top: var(--bs-space-2); }
        .hint { font-size: 0.75rem; color: var(--bs-text-muted); margin-top: 0.375rem; max-width: 10rem; }
        .panel .hint { max-width: none; margin: calc(-1 * var(--bs-space-3)) 0 var(--bs-space-4); }

        tbody tr.sub { border-top: 0; }
        tr.sub td { padding-top: 0; }
        tr.sub .error { margin-bottom: 0; }
        @media (max-width: 860px) { .columns { grid-template-columns: minmax(0, 1fr); gap: var(--bs-space-6); } }
        h1 { margin-bottom: var(--bs-space-5); }
      </style>

      <h1>People</h1>

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
                           <th scope="col">Roles</th>
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

          <fieldset class="bare">
            <legend class="label">Roles</legend>
            ${roleBoxes(this.#form, { prefix: "add" })}
          </fieldset>
          ${hasAccess(this.#form) ? "" : `<p class="hint">Pick at least one role.</p>`}

          ${this.#formError ? `<p class="error">${escapeHTML(this.#formError.message)}</p>` : ""}
          <button class="btn btn--navy btn--small btn--block" ${this.#busy || !hasAccess(this.#form) ? "disabled" : ""}>
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
        <td>${roleChips(person)}</td>
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
    const hint = !hasAccess(draft)
      ? "Pick at least one role."
      : lastAdmin
        ? "The only admin has to stay an admin."
        : this.#isSelf(person) && !draft.admin
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
          <fieldset class="bare">
            <legend class="visually-hidden">Roles</legend>
            ${roleBoxes(draft, { prefix: "edit", compact: true, keepAdmin: lastAdmin })}
          </fieldset>
          ${hint ? `<p class="hint">${hint}</p>` : ""}
        </td>
        <td class="added">${escapeHTML(longDate(person.addedOn))}</td>
        <td class="actions">
          <button class="btn btn--navy btn--tiny" form="edit-person" ${this.#rowBusy || !hasAccess(draft) ? "disabled" : ""}>
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

const blankForm = () => ({ name: "", email: "", admin: false, roles: [FIELD_VOLUNTEER] });

/** A volunteer can't be saved without a role: they could sign in to nothing. */
const hasAccess = (draft) => draft.admin || draft.roles.length > 0;

/** A form's draft as the API takes it. */
const bodyOf = ({ name, email, admin, roles }) => ({ name, email, role: admin ? ADMIN : "volunteer", roles });

/** Ticks or clears the role a checkbox stands for, in a draft. */
function toggle(draft, el) {
  if (el.value === ADMIN) {
    draft.admin = el.checked;
    return;
  }
  draft.roles = el.checked ? [...new Set([...draft.roles, el.value])] : draft.roles.filter((r) => r !== el.value);
}

/**
 * A draft's roles as checkboxes: the volunteer roles, then Admin. The add form
 * says what each is for; a row being edited has only room for the names.
 * keepAdmin fixes Admin on, for the last one.
 */
function roleBoxes(draft, { prefix, compact = false, keepAdmin = false }) {
  const box = ({ id, label, what }, { checked, disabled = false, title = "", extra = "" }) => `
    <label class="${extra}" ${title ? `title="${escapeHTML(title)}"` : ""}>
      <input type="checkbox" id="${prefix}-${id}" value="${id}" data-change="role"
             ${prefix === "edit" ? 'data-draft="1" form="edit-person"' : ""}
             ${checked ? "checked" : ""} ${disabled ? "disabled" : ""} />
      <span>${label}${compact ? "" : `<span class="what">${what}</span>`}</span>
    </label>`;
  const expert = draft.roles.includes(BIRDING_EXPERT);
  return `
    <div class="roles ${compact ? "roles--row" : ""}">
      ${VOLUNTEER_ROLES.map((role) =>
        role.id === REVIEWER && expert
          ? box(role, { checked: true, disabled: true, title: "A birding expert reviews too" })
          : box(role, { checked: draft.roles.includes(role.id) }),
      ).join("")}
      ${box(
        { id: ADMIN, label: "Admin", what: "Everything, and manages people, recorders and every card" },
        { checked: draft.admin, disabled: keepAdmin && draft.admin, extra: "admin" },
      )}
    </div>`;
}

/** What someone holds, as chips: Admin first, in navy, then their volunteer roles. */
function roleChips(person) {
  return `<div class="chips">${labels(person)
    .map((label) => `<bs-chip kind="${label === "Admin" ? "admin" : "neutral"}">${label}</bs-chip>`)
    .join("")}</div>`;
}

customElements.define("bs-admin-people", AdminPeople);
