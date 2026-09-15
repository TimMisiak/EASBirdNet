import { BaseElement } from "./base-element.js";
import { navigate, onNavigate, path } from "../router.js";
import * as session from "../session.js";
import "./bs-site-header.js";
import "./bs-site-footer.js";
import "./bs-app-header.js";

/**
 * <bs-app> is the shell: it picks the page for the current route, wraps it in
 * the right chrome, and keeps anyone who isn't signed in out of the app.
 *
 * Routes are grouped by chrome rather than by feature -- the public pages get
 * the masthead and footer, everything behind sign-in gets the app header.
 */
const ROUTES = [
  { path: "/", tag: "bs-home-page", chrome: "public" },
  { path: "/signin", tag: "bs-signin-page", chrome: "bare" },
  { path: "/app", redirect: "/app/upload" },
  // The volunteer's tabs. Upload holds the four steps of sending a card.
  { path: "/app/upload", tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/app/upload/check", tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/app/upload/progress", tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/app/upload/done", tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/app/uploads", tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/app/detections", tag: "bs-volunteer-page", chrome: "app", auth: true },
  // One detection, opened from that list: /app/detections/OWL-20260914-SR03/det_….
  { path: "/app/detections/", prefix: true, tag: "bs-volunteer-page", chrome: "app", auth: true },
  { path: "/admin", redirect: "/admin/people" },
  { path: "/admin/people", tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
  { path: "/admin/recorders", tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
  { path: "/admin/uploads", tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
  // One card: /admin/uploads/OWL-20260914-SR03.
  { path: "/admin/uploads/", prefix: true, tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
  { path: "/admin/detections", tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
  // One detection, opened from that list: /admin/detections/OWL-20260914-SR03/det_….
  { path: "/admin/detections/", prefix: true, tag: "bs-admin-page", chrome: "app", auth: true, admin: true },
];

/** The route for a path: an exact match, or a prefix route with something after the prefix. */
const routeFor = (here) =>
  ROUTES.find((r) => (r.prefix ? here.startsWith(r.path) && here.length > r.path.length : r.path === here));

class BirdsenseApp extends BaseElement {
  #route = null;

  connectedCallback() {
    super.connectedCallback();
    onNavigate(() => this.render());
    session.onChange(() => this.render());
    session.load();
  }

  render() {
    if (!session.isLoaded()) {
      // Routing before the session is known would bounce a signed-in volunteer
      // to /signin for a frame. Hold the frame instead.
      this.shadowRoot.innerHTML = `<style>:host{display:block;min-height:100vh;background:var(--bs-bg);}</style>`;
      return;
    }

    const route = routeFor(path());

    if (route?.redirect) return navigate(route.redirect, { replace: true });
    if (route?.auth && !session.isSignedIn()) return navigate("/signin", { replace: true });
    if (route?.admin && !session.isAdmin()) return navigate("/app", { replace: true });

    this.#route = route;
    this.shadowRoot.innerHTML = `
      <style>
        :host { display: flex; flex-direction: column; min-height: 100vh; }
        main.app {
          flex: 1;
          width: 100%;
          max-width: var(--bs-measure);
          margin: 0 auto;
          padding: var(--bs-space-7) var(--bs-space-6) var(--bs-space-8);
        }
        main.public { flex: 1; }
        .missing {
          flex: 1;
          display: grid;
          place-content: center;
          gap: var(--bs-space-4);
          text-align: center;
          padding: var(--bs-space-8) var(--bs-space-4);
        }
        .missing h1 { font-family: var(--bs-font-display); font-weight: 400; font-size: 2rem; margin: 0; }
        @media (max-width: 720px) {
          main.app { padding: var(--bs-space-5) var(--bs-space-4) var(--bs-space-7); }
        }
      </style>
      ${this.#chrome(route)}
    `;
  }

  #chrome(route) {
    if (!route) {
      return `
        <bs-site-header></bs-site-header>
        <div class="missing">
          <h1>That page isn't here.</h1>
          <p><a href="/">Back to the detections page</a></p>
        </div>
        <bs-site-footer></bs-site-footer>
      `;
    }
    if (route.chrome === "bare") return `<${route.tag}></${route.tag}>`;
    if (route.chrome === "public") {
      return `
        <bs-site-header></bs-site-header>
        <main class="public"><${route.tag}></${route.tag}></main>
        <bs-site-footer></bs-site-footer>
      `;
    }
    return `
      <bs-app-header></bs-app-header>
      <main class="app"><${route.tag}></${route.tag}></main>
    `;
  }
}

customElements.define("bs-app", BirdsenseApp);
