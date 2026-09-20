// Entry point. Its only job is to import every component module so their
// customElements.define() calls run; nothing here bootstraps the page. The page
// starts when the browser upgrades <bs-app> in index.html.
//
// <bs-app> imports the chrome it always needs; the page components are listed
// here because it names them as strings in its route table.
import "./components/bs-app.js";
import "./components/bs-home-page.js";
import "./components/bs-signin-page.js";
import "./components/bs-app-page.js";
