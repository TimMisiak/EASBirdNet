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
 *
 * A route also carries its `title`, which is what the tab and the history entry
 * say. It lives here rather than in each page because this is the one place
 * that knows a path became a page: a page that fetches before it can draw would
 * otherwise leave the tab naming the page before it.
 */
const SITE = "Birdsense — Eastside Audubon";
const ROUTES = [
  { path: "/", tag: "bs-home-page", chrome: "public", title: SITE },
  { path: "/signin", tag: "bs-signin-page", chrome: "bare", title: "Sign in" },
  { path: "/app", redirect: "/app/upload" },
  // Everyone signed in gets <bs-app-page> and its one row of tabs; a
  // coordinator's three extra tabs keep the /admin/ paths the guard reads.
  { path: "/app/upload", tag: "bs-app-page", chrome: "app", auth: true, title: "Upload a card" },
  { path: "/app/upload/check", tag: "bs-app-page", chrome: "app", auth: true, title: "Check the card" },
  { path: "/app/upload/progress", tag: "bs-app-page", chrome: "app", auth: true, title: "Uploading the card" },
  { path: "/app/upload/done", tag: "bs-app-page", chrome: "app", auth: true, title: "Card uploaded" },
  { path: "/app/uploads", tag: "bs-app-page", chrome: "app", auth: true, title: "My uploads" },
  { path: "/app/detections", tag: "bs-app-page", chrome: "app", auth: true, title: "Detections" },
  // One detection, opened from that list: /app/detections/OWL-20260914-SR03/det_….
  { path: "/app/detections/", prefix: true, tag: "bs-app-page", chrome: "app", auth: true, title: "Detection" },
  { path: "/admin", redirect: "/admin/uploads" },
  { path: "/admin/uploads", tag: "bs-app-page", chrome: "app", auth: true, admin: true, title: "All uploads" },
  // One card: /admin/uploads/OWL-20260914-SR03.
  { path: "/admin/uploads/", prefix: true, tag: "bs-app-page", chrome: "app", auth: true, admin: true, title: "Card" },
  { path: "/admin/recorders", tag: "bs-app-page", chrome: "app", auth: true, admin: true, title: "Recorders" },
  { path: "/admin/people", tag: "bs-app-page", chrome: "app", auth: true, admin: true, title: "People" },
  // Detections is one tab for everyone now. A link a coordinator sent while it
  // was two lands on it, filters and all.
  { path: "/admin/detections", redirect: "/app/detections" },
  { path: "/admin/detections/", prefix: true, redirect: (here) => `/app/detections/${here.slice("/admin/detections/".length)}` },
];

/**
 * The route for a path: an exact match, or a prefix route with something after
 * the prefix.
 *
 * A path with a malformed percent-escape in it (/admin/uploads/%zz) has no
 * route, so it lands on the "that page isn't here" screen. Every page under a
 * prefix route decodes the id off the end, and decodeURIComponent throws on
 * one of those -- which, thrown out of render(), leaves a blank page instead.
 */
const routeFor = (here) => {
  try {
    decodeURIComponent(here);
  } catch {
    return undefined;
  }
  return ROUTES.find((r) => (r.prefix ? here.startsWith(r.path) && here.length > r.path.length : r.path === here));
};

class BirdsenseApp extends BaseElement {
  /** The path the last drawn page was for, so a query-only change doesn't count as arriving. */
  #at = null;

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

    const here = path();
    const route = routeFor(here);

    if (route?.redirect) {
      // A redirect is a path, or a function of the path for a route with an id
      // on the end. Either way it keeps the query string the link carried.
      const to = typeof route.redirect === "function" ? route.redirect(here) : route.redirect;
      return navigate(`${to}${location.search}`, { replace: true });
    }
    if (route?.auth && !session.isSignedIn()) {
      // A session that ended under them says so, in the same way the OIDC
      // callback reports a sign-in that didn't work.
      const to = session.wasEnded() ? "/signin?error=session-ended" : "/signin";
      return navigate(to, { replace: true });
    }
    if (route?.admin && !session.isAdmin()) return navigate("/app", { replace: true });

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
        /* Focused only to move the reader onto the new page; the ring belongs
           to what they tab to next, not to the page itself. */
        [data-page]:focus { outline: none; }
        @media (max-width: 720px) {
          main.app { padding: var(--bs-space-5) var(--bs-space-4) var(--bs-space-7); }
        }
      </style>
      ${this.#chrome(route)}
    `;
    this.#arrived(here, route);
  }

  /**
   * What the browser does when a route becomes a page, which it does for
   * itself on a full page load and for nobody on a history one: name the page,
   * put the reader at the top of it, and move the focus into it so the next Tab
   * -- and a screen reader -- is on the new page rather than back in the tab
   * strip of the old one.
   *
   * Only a change of path counts. A page that keeps its filters in the query
   * string (the detections list) rewrites it as the reader works, and that is
   * the same page, still where they left it.
   */
  #arrived(here, route) {
    document.title = !route ? `Page not found · ${SITE}` : route.title === SITE ? SITE : `${route.title} · ${SITE}`;
    const first = this.#at === null;
    if (here === this.#at) return;
    this.#at = here;
    if (first) return;
    window.scrollTo(0, 0);
    // preventScroll, or the focus scrolls the page far enough to bring the
    // region's own top edge up -- past the header, and past the top we just
    // went to.
    this.$("[data-page]")?.focus({ preventScroll: true });
  }

  #chrome(route) {
    if (!route) {
      return `
        <bs-site-header></bs-site-header>
        <div class="missing" data-page tabindex="-1">
          <h1>That page isn't here.</h1>
          <p><a href="/">Back to the home page</a></p>
        </div>
        <bs-site-footer></bs-site-footer>
      `;
    }
    if (route.chrome === "bare") return `<${route.tag} data-page tabindex="-1"></${route.tag}>`;
    if (route.chrome === "public") {
      return `
        <bs-site-header></bs-site-header>
        <main class="public" data-page tabindex="-1"><${route.tag}></${route.tag}></main>
        <bs-site-footer></bs-site-footer>
      `;
    }
    return `
      <bs-app-header></bs-app-header>
      <main class="app" data-page tabindex="-1"><${route.tag}></${route.tag}></main>
    `;
  }
}

customElements.define("bs-app", BirdsenseApp);
