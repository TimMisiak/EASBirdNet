// The list of every card's detections: how its view is read off a query
// string, how that view is asked for, and the pages of it that have been
// fetched.
//
// <bs-detections> shows the list and <bs-detection-detail> steps through it,
// and neither survives a navigation -- the shell rewrites its shadow root, so
// both elements are built again for every route. What they share therefore
// lives here, in the module, which does survive.
//
// That is also what makes Previous and Next cheap. Stepping from one detection
// to the next is stepping through the page of rows the reviewer opened one
// from, which is already in hand; only running off the end of a page costs a
// request, and then only for the one page next door. Nothing re-runs the
// search.
//
// The pages held are a snapshot on purpose: a reviewer working through
// "unreviewed" would otherwise have the list shift under them with every
// verdict. Going back to the list fetches it again.

import * as api from "./api.js";

/** Rows per page, both in the list and in what stepping moves through. */
export const PAGE_SIZE = 50;

/**
 * How far back the list looks until the date fields say otherwise, matching
 * the server's own window (api.defaultDetectionsDays).
 *
 * A week, not a season: a recorder yields on the order of 4,000 detections a
 * day, and the server has to read every row a filter matches before it can
 * sort or page it (SCHEMA.md), so a wider opening view than this is one the
 * replica can't answer. Widening it is a date field away, and a range too wide
 * to read says so rather than failing.
 */
export const DEFAULT_DAYS = 7;

export const STATUSES = [
  { id: "", label: "All" },
  { id: "unreviewed", label: "Unreviewed" },
  { id: "confirmed", label: "Confirmed" },
  { id: "rejected", label: "Discarded" },
];

/** Minimum confidence choices, in percent; 0 is any. */
export const CONFIDENCES = [0, 50, 70, 80, 90];

/** The columns that sort, and which way each starts. */
export const SORTS = { heard: "desc", species: "asc", confidence: "desc" };

const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** A date as YYYY-MM-DD in the browser's timezone, which is what a date field takes. */
function dayOf(date) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

/** The dates the list starts on: the DEFAULT_DAYS days up to today. */
export function defaultDates(now = new Date()) {
  const from = new Date(now);
  from.setDate(from.getDate() - DEFAULT_DAYS);
  return { from: dayOf(from), to: dayOf(now) };
}

/**
 * The list's view as a query string gives it, anything unreadable left at its
 * default. dates are the range a query string that names no readable ones of
 * its own gets, so a bare /app/detections is the past DEFAULT_DAYS days.
 */
export function viewFrom(params, dates = defaultDates()) {
  const sort = Object.hasOwn(SORTS, params.get("sort")) ? params.get("sort") : "heard";
  const order = params.get("order") === "asc" || params.get("order") === "desc" ? params.get("order") : SORTS[sort];
  const min = Number(params.get("min"));
  const status = params.get("status") ?? "";
  return {
    status: STATUSES.some((s) => s.id === status) ? status : "",
    species: params.get("species") ?? "",
    min: CONFIDENCES.includes(min) ? min : 0,
    from: DATE.test(params.get("from")) ? params.get("from") : dates.from,
    to: DATE.test(params.get("to")) ? params.get("to") : dates.to,
    sort,
    order,
    page: Math.max(1, Number.parseInt(params.get("page"), 10) || 1),
  };
}

/** The query string for a view, leaving out what is at its default. */
export function paramsFor(view) {
  const params = new URLSearchParams();
  if (view.status) params.set("status", view.status);
  if (view.species) params.set("species", view.species);
  if (view.min) params.set("min", view.min);
  if (view.from) params.set("from", view.from);
  if (view.to) params.set("to", view.to);
  if (view.sort !== "heard") params.set("sort", view.sort);
  if (view.order !== SORTS[view.sort]) params.set("order", view.order);
  if (view.page > 1) params.set("page", view.page);
  return params;
}

/** Midnight at the start of a YYYY-MM-DD day, in the browser's timezone. */
function startOfDay(date, addDays = 0) {
  const d = new Date(`${date}T00:00:00`);
  d.setDate(d.getDate() + addDays);
  return d;
}

/** What GET /detections is asked for one page of a view. */
function queryFor(view) {
  const params = { sort: view.sort, order: view.order, limit: PAGE_SIZE, offset: (view.page - 1) * PAGE_SIZE };
  if (view.status) params.status = view.status;
  if (view.species) params.species = view.species;
  if (view.min) params.minConfidence = view.min / 100;
  if (view.from) params.since = startOfDay(view.from).toISOString();
  if (view.to) params.until = startOfDay(view.to, 1).toISOString();
  return params;
}

/** The filters and the sort of a view, without which page of it. */
export function keyOf(view) {
  const params = paramsFor(view);
  params.delete("page");
  return params.toString();
}

/**
 * The list as last asked for: key is what keyOf says, so a page can join what
 * is already held and anything under a different key starts the held pages
 * again; pages is what has been fetched, by page number.
 */
const held = { key: "", total: 0, pages: new Map() };

/** How many pages to keep. Stepping needs a page and the ones either side. */
const KEEP = 4;

/** Ask for one page of a view, and hold it. Returns the server's whole answer. */
export async function loadPage(view) {
  const body = await api.fetchDetections(queryFor(view));
  hold(view, body);
  return body;
}

function hold(view, { detections, total }) {
  const key = keyOf(view);
  if (key !== held.key) {
    held.key = key;
    held.pages.clear();
  }
  held.total = total;
  // Re-inserting puts this page last, so the trim below drops the pages the
  // reviewer has stepped furthest away from.
  held.pages.delete(view.page);
  held.pages.set(view.page, detections);
  for (const page of held.pages.keys()) {
    if (held.pages.size <= KEEP) break;
    if (page !== view.page) held.pages.delete(page);
  }
}

/** Where a detection sits in the whole list, from the pages held, or null. */
function indexOf(reference, id) {
  for (const [page, rows] of held.pages) {
    const at = rows.findIndex((d) => d.id === id && d.reference === reference);
    if (at >= 0) return (page - 1) * PAGE_SIZE + at;
  }
  return null;
}

/** The row at a place in the list, fetching its page if it isn't held. */
async function rowAt(view, index) {
  const page = Math.floor(index / PAGE_SIZE) + 1;
  let rows = held.pages.get(page);
  if (!rows) {
    // Stepping is a convenience; a page that won't load just ends it.
    rows = await loadPage({ ...view, page }).then((body) => body.detections, () => null);
    if (!rows) return null;
  }
  const row = rows[index % PAGE_SIZE];
  return row ? { reference: row.reference, id: row.id, page } : null;
}

/**
 * Where a detection sits in the list a view describes, and what is either
 * side of it: {index, total, prev, next}, each neighbour {reference, id,
 * page}. The page is which one of the list the neighbour is on, so a link to
 * it carries the list back to where the reviewer would find the row.
 *
 * null when that list isn't the one in hand -- a detection opened from a link
 * rather than from the list, or filters that have moved on since. Then there
 * is nothing to step through but the file's own detections, which is what the
 * detection page falls back to.
 */
export async function place(view, reference, id) {
  if (keyOf(view) !== held.key) return null;
  const index = indexOf(reference, id);
  if (index === null) return null;
  // Only one of the two can be on a page that isn't held, so this is at most
  // one request, and it runs beside the detection's own.
  const [prev, next] = await Promise.all([
    index > 0 ? rowAt(view, index - 1) : null,
    index + 1 < held.total ? rowAt(view, index + 1) : null,
  ]);
  return { index, total: held.total, prev, next };
}

/** Keep a verdict given on the detection page in the rows already held. */
export function reviewed(reference, saved) {
  for (const rows of held.pages.values()) {
    const at = rows.findIndex((d) => d.id === saved.id && d.reference === reference);
    if (at >= 0) rows[at] = { ...rows[at], reviewStatus: saved.reviewStatus, review: saved.review };
  }
}
