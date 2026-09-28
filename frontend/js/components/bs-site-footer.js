import { BaseElement } from "./base-element.js";

// The two of the main site's footer links that matter to a Birdsense visitor.
// Volunteering is Eastside Audubon's sign-up form on Neon CRM; contact is on
// eastsideaudubon.org.
const LINKS = [
  ["Volunteer", "https://eastsideaudubon.app.neoncrm.com/forms/volunteer"],
  ["Contact us", "https://www.eastsideaudubon.org/contact"],
];

/**
 * <bs-site-footer> is the navy footer at the bottom of every page, modelled on
 * the eastsideaudubon.org footer: a short set of its links, then the BirdNET
 * credit the models' CC BY-NC-SA licence asks for (see THIRD_PARTY_NOTICES.md).
 */
class SiteFooter extends BaseElement {
  render() {
    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: block;
          background: var(--bs-navy);
          color: var(--bs-on-navy-muted);
          font-size: 0.9375rem;
          text-align: center;
          /* The copper accent is too dark against navy to show focus; white isn't. */
          --bs-focus: var(--bs-on-navy);
        }
        .inner {
          max-width: 60rem;
          margin: 0 auto;
          padding: 3.25rem var(--bs-space-6) var(--bs-space-7);
        }
        nav {
          display: flex;
          flex-wrap: wrap;
          justify-content: center;
          gap: var(--bs-space-3) 2rem;
          padding-bottom: var(--bs-space-6);
          border-bottom: 1px solid rgba(255, 255, 255, 0.15);
        }
        nav a {
          color: var(--bs-on-navy);
          font-size: 0.8125rem;
          letter-spacing: 0.15em;
          text-transform: uppercase;
          text-decoration: none;
        }
        nav a:hover { text-decoration: underline; text-underline-offset: 4px; }
        .fine { padding-top: var(--bs-space-5); font-size: 0.8125rem; }
        p { margin: 0; line-height: 1.6; }
        p a { color: inherit; text-underline-offset: 2px; }
        p a:hover { color: var(--bs-on-navy); }

        @media (max-width: 720px) {
          .inner { padding: var(--bs-space-7) var(--bs-space-4); }
          nav { gap: var(--bs-space-3) 1.25rem; }
        }
      </style>
      <footer class="inner">
        <nav aria-label="Eastside Audubon">
          ${LINKS.map(([label, href]) => `<a href="${href}">${label}</a>`).join("")}
        </nav>
        <div class="fine">
          <p>
            Bird calls identified with
            <a href="https://birdnet.cornell.edu/" target="_blank" rel="noreferrer">BirdNET</a>,
            by the K. Lisa Yang Center for Conservation Bioacoustics, Cornell Lab of Ornithology,
            and Chemnitz University of Technology. BirdNET models are licensed
            <a href="https://creativecommons.org/licenses/by-nc-sa/4.0/" target="_blank" rel="noreferrer">CC BY-NC-SA 4.0</a>.
          </p>
        </div>
      </footer>
    `;
  }
}

customElements.define("bs-site-footer", SiteFooter);
