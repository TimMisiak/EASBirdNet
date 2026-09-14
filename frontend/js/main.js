// Entry point. Its only job is to import every component module so their
// customElements.define() calls run; nothing here bootstraps the page. The page
// starts when the browser upgrades <bs-app> in index.html.
import "./components/bs-app.js";
import "./components/bs-detection-list.js";
import "./components/bs-detection-card.js";
