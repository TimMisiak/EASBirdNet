// Constructable stylesheets shared across components.
//
// Shadow DOM is the point of these components, but it also means a <style>
// block is per-component. The primitives below -- buttons, tables, form fields,
// panels -- appear on nearly every screen, and copying them into a dozen files
// is how they drift. Components adopt what they need via `static styles`.
//
// Everything here still styles itself with var(--bs-*): these sheets share
// rules, not values.

// Adopted stylesheets are ordered *after* a shadow root's own <style>, so an
// unlayered shared rule would beat the component that adopted it. A cascade
// layer inverts that: anything a component writes for itself is unlayered, and
// unlayered rules win over every layer.
const sheet = (css) => {
  const s = new CSSStyleSheet();
  s.replaceSync(`@layer bs-base { ${css} }`);
  return s;
};

/**
 * The one sheet every component gets, adopted by BaseElement itself: what has
 * to hold inside every shadow root, because a document stylesheet doesn't
 * reach in.
 *
 * `* { box-sizing: border-box }` in app.css stops at the shadow boundary, and a
 * component whose fields are content-box overflows its own column. The UA's
 * `[hidden]` rule loses to any `display` a component sets, so `el.hidden` needs
 * the `!important` form to mean anything. `.visually-hidden` is text for a
 * screen reader and nobody else -- the label on an actions column, say.
 *
 * The focus ring is here for the same reason as box-sizing: `outline` isn't
 * inherited, so the document's rule reaches nothing inside a shadow root, and
 * every button and link in the app would fall back to the UA ring -- which is
 * what the tokens exist to replace, and what disappears against forest green.
 */
export const reset = sheet(`
  *, *::before, *::after { box-sizing: border-box; }
  [hidden] { display: none !important; }
  :focus-visible { outline: 2px solid var(--bs-focus); outline-offset: 2px; }
  .visually-hidden {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
`);

/** Typography and links. Adopted by essentially everything. */
export const typography = sheet(`
  :host { display: block; }
  h1, h2, h3 {
    font-family: var(--bs-font-display);
    font-weight: 400;
    letter-spacing: -0.01em;
    margin: 0;
    text-wrap: pretty;
  }
  h1 { font-size: clamp(1.9rem, 1.3rem + 2vw, 2.375rem); }
  h2 { font-size: clamp(1.5rem, 1.2rem + 1vw, 1.625rem); }
  h3 { font-size: 1.25rem; font-weight: 500; }
  p { margin: 0; text-wrap: pretty; }
  a { color: var(--bs-link); text-decoration: underline; text-underline-offset: 2px; }
  a:hover { color: var(--bs-link-hover); }

  /* The small mono line the design uses to label a section or a reference. */
  .eyebrow {
    font-family: var(--bs-font-mono);
    font-size: 0.6875rem;
    letter-spacing: 0.16em;
    text-transform: uppercase;
    color: var(--bs-text-muted);
  }
  .mono { font-family: var(--bs-font-mono); }
  .muted { color: var(--bs-text-muted); }
  .lede { font-size: 0.9375rem; color: var(--bs-text-quiet); line-height: 1.6; }
`);

/** Buttons and link-shaped buttons. */
export const controls = sheet(`
  button {
    font: inherit;
    color: inherit;
    border-radius: var(--bs-radius);
    cursor: pointer;
    transition: background-color 120ms ease, border-color 120ms ease;
  }
  button[disabled] { cursor: not-allowed; opacity: 0.55; }

  .btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--bs-space-2);
    padding: 0.9375rem 1.5rem;
    font-size: 0.9375rem;
    border: 1px solid transparent;
    white-space: nowrap;
  }
  .btn--primary { background: var(--bs-amber); color: var(--bs-text); font-weight: 500; }
  .btn--primary:hover:not([disabled]) { background: var(--bs-amber-hover); }
  .btn--forest { background: var(--bs-forest); color: var(--bs-on-forest); }
  .btn--forest:hover:not([disabled]) { background: var(--bs-forest-hover); }
  .btn--quiet { background: transparent; border-color: var(--bs-border-strong); }
  .btn--quiet:hover:not([disabled]) { border-color: var(--bs-forest); }
  .btn--small { padding: 0.6875rem 1.125rem; font-size: 0.875rem; }
  /* Small enough to sit in a table row without setting its height. */
  .btn--tiny { padding: 0.375rem 0.75rem; font-size: 0.8125rem; }
  .btn--block { width: 100%; }

  /* Destructive actions. Quiet until asked, solid inside the confirmation. */
  .btn--danger { color: var(--bs-chip-attention-text); }
  .btn--danger:hover:not([disabled]):not([aria-disabled="true"]) { border-color: var(--bs-chip-attention-text); }
  .btn--danger-solid {
    background: var(--bs-chip-attention-text);
    border-color: var(--bs-chip-attention-text);
    color: var(--bs-surface);
  }
  /* Looks disabled but still takes a click, so it can say why it is. */
  .btn[aria-disabled="true"] { opacity: 0.55; cursor: not-allowed; }
  .btn[aria-disabled="true"]:hover { border-color: var(--bs-border-strong); }
`);

/** Text inputs, selects, textareas and their labels. */
export const forms = sheet(`
  input, select, textarea { font: inherit; color: inherit; }
  label { display: block; }
  .label {
    font-size: 0.84375rem;
    font-weight: 500;
    margin-bottom: 0.4375rem;
  }
  .label .optional { font-weight: 400; color: var(--bs-text-muted); }
  .field {
    width: 100%;
    min-height: 3rem;
    padding: 0 var(--bs-space-3);
    background: var(--bs-surface);
    border: 1px solid var(--bs-border-strong);
    border-radius: var(--bs-radius);
    font-size: 0.9375rem;
  }
  textarea.field { padding: var(--bs-space-3); line-height: 1.5; resize: vertical; }
  .field--sunk { background: var(--bs-field); min-height: 2.875rem; font-size: 0.90625rem; }
  .field--mono { font-family: var(--bs-font-mono); font-size: 0.84375rem; }
  .field:focus-visible { border-color: var(--bs-forest); }
  /* A value the volunteer doesn't set: derived, shown, not editable. */
  .readout {
    display: flex;
    align-items: center;
    gap: var(--bs-space-2);
    min-height: 3rem;
    padding: 0 var(--bs-space-3);
    background: var(--bs-surface-sunk);
    border: 1px solid var(--bs-border);
    border-radius: var(--bs-radius);
    font-size: 0.9375rem;
  }
  .tag {
    font-size: 0.71875rem;
    color: var(--bs-text-muted);
    border: 1px solid var(--bs-border-strong);
    border-radius: var(--bs-radius-pill);
    padding: 0.125rem 0.5625rem;
    white-space: nowrap;
  }
  .error {
    color: var(--bs-chip-attention-text);
    background: var(--bs-chip-attention-bg);
    border: 1px solid var(--bs-chip-attention-border);
    border-radius: var(--bs-radius);
    padding: var(--bs-space-2) var(--bs-space-3);
    font-size: 0.875rem;
  }
`);

/**
 * Data tables. The design's tables are rules and whitespace, no fill; the
 * header is a mono all-caps line rather than a shaded band.
 */
export const tables = sheet(`
  table { width: 100%; border-collapse: collapse; }
  thead tr {
    font-family: var(--bs-font-mono);
    font-size: 0.65625rem;
    letter-spacing: 0.14em;
    text-transform: uppercase;
    color: var(--bs-text-muted);
    text-align: left;
  }
  th { font-weight: 400; padding: var(--bs-space-4) var(--bs-space-3) var(--bs-space-3); }
  td { padding: var(--bs-space-4) var(--bs-space-3); vertical-align: top; }
  th:first-child, td:first-child { padding-left: 0; }
  th:last-child, td:last-child { padding-right: 0; }
  tbody tr { border-top: 1px solid var(--bs-border); }
  .num { text-align: right; font-family: var(--bs-font-mono); font-size: 0.875rem; }
  /* Tables scroll inside their own box rather than the page. */
  .table-scroll { overflow-x: auto; }
`);

/**
 * The row of pills above a list that narrows it, and the count of what is in
 * it. The pills are `aria-pressed` buttons, not links: they filter what is on
 * screen rather than changing the route.
 */
export const filters = sheet(`
  .filters {
    display: flex;
    align-items: center;
    gap: var(--bs-space-3);
    margin-bottom: var(--bs-space-4);
    flex-wrap: wrap;
  }
  .filter {
    font-size: 0.8125rem;
    padding: 0.375rem 0.875rem;
    border-radius: var(--bs-radius-pill);
    border: 1px solid var(--bs-border-strong);
    background: transparent;
    color: var(--bs-text-body);
  }
  .filter[aria-pressed="true"] { background: var(--bs-text); border-color: var(--bs-text); color: var(--bs-on-forest); }
  /* What the list holds, pushed to the end of the row. */
  .tally { margin-left: auto; font-size: 0.8125rem; color: var(--bs-text-muted); }
`);

/** Bordered boxes: the white panel, the parchment aside, the amber notice. */
export const panels = sheet(`
  .panel {
    background: var(--bs-surface);
    border: 1px solid var(--bs-border);
    padding: var(--bs-space-5) var(--bs-space-6);
  }
  .panel--parchment { background: var(--bs-parchment); border-color: var(--bs-parchment-border); }
  .panel--notice {
    background: var(--bs-notice);
    border-color: var(--bs-notice-border);
    color: var(--bs-notice-text);
  }
  .panel h3 { margin-bottom: var(--bs-space-3); }
  .panel p { font-size: 0.875rem; line-height: 1.6; color: var(--bs-text-body); }
  .panel--notice p { color: var(--bs-notice-text); }
  .stack { display: flex; flex-direction: column; gap: var(--bs-space-4); }
  .row { display: flex; gap: var(--bs-space-3); flex-wrap: wrap; }
  /* The rule-under-the-heading that opens most sections. */
  .section-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--bs-space-5);
    flex-wrap: wrap;
    border-bottom: 2px solid var(--bs-text);
    padding-bottom: var(--bs-space-3);
  }
  /* The footer rule every wizard step ends on. */
  .step-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--bs-space-5);
    flex-wrap: wrap;
    margin-top: var(--bs-space-7);
    padding-top: var(--bs-space-5);
    border-top: 1px solid var(--bs-border);
  }
  .note { font-size: 0.8125rem; color: var(--bs-text-muted); line-height: 1.6; }

  /* An inline "are you sure?" before something is deleted. */
  .confirm {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--bs-space-3);
    flex-wrap: wrap;
    padding: var(--bs-space-3) var(--bs-space-4);
    background: var(--bs-chip-attention-bg);
    border: 1px solid var(--bs-chip-attention-border);
    border-radius: var(--bs-radius);
    color: var(--bs-chip-attention-text);
    font-size: 0.875rem;
  }
  .confirm p { color: var(--bs-chip-attention-text); margin: 0; }
  .confirm .row { gap: var(--bs-space-2); }
`);

/**
 * A page's tabs, which are routes: <bs-app-page> opens with a row of them, the
 * coordinator's included. The current one carries aria-current="page".
 */
export const tabs = sheet(`
  .tabs {
    display: flex;
    gap: var(--bs-space-1);
    /* The baseline is an inset shadow, not a border: a tab's own underline sits
       on top of it without a negative margin, so nothing overflows the box and
       overflow-x below can't earn a vertical scrollbar for a stray pixel. */
    box-shadow: inset 0 -1px 0 var(--bs-border);
    margin-bottom: 2.125rem;
    overflow-x: auto;
  }
  .tab {
    border-bottom: 2px solid transparent;
    color: var(--bs-text-muted);
    padding: 0.625rem var(--bs-space-4);
    font-size: 0.90625rem;
    text-decoration: none;
    white-space: nowrap;
  }
  .tab:hover { color: var(--bs-text); }
  .tab[aria-current="page"] { border-bottom-color: var(--bs-amber); color: var(--bs-text); }
`);
