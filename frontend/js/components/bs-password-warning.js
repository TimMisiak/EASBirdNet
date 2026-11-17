import { BaseElement } from "./base-element.js";
import { panels, typography } from "../shared-styles.js";

/**
 * <bs-password-warning> tells an admin what they give up with a password. It
 * warns and leaves the choice with them. It only ever shows to someone who has
 * proved who they are -- a good link, or the right password -- because showing
 * it sooner would tell a stranger which addresses are admins'.
 */
class PasswordWarning extends BaseElement {
  static styles = [typography, panels];

  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; }
        strong { display: block; margin-bottom: var(--bs-space-2); }
      </style>
      <div class="panel panel--notice" role="note">
        <p>
          <strong>You're an admin, so a password is riskier for you.</strong>
          Anyone who gets it can delete cards and change who has access, and
          it has no second step. Google and Microsoft can ask for one, so
          signing in with them is safer if you have an account there.
        </p>
      </div>
    `;
  }
}

customElements.define("bs-password-warning", PasswordWarning);
