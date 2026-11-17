// Thin wrapper over the JSON API. Components call these functions instead of
// fetch() directly, so error handling and the /api/v1 prefix live in one place.

const BASE = "/api/v1";

/** Thrown for any non-2xx response, carrying the server's own message. */
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/**
 * Told when the server says a session has ended, so one 401 anywhere signs the
 * whole app out. session.js sets it at startup; api.js can't import session.js,
 * which imports this, and api.js is the module everything else is built on.
 */
let signedOut = () => {};
export const onUnauthorized = (handler) => {
  signedOut = handler;
};

async function request(method, path, body) {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: body
      ? { Accept: "application/json", "Content-Type": "application/json" }
      : { Accept: "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });

  // Every handler answers in JSON, including its errors -- but a proxy or a
  // dropped connection might not, so don't assume the body parses.
  const payload = await res.json().catch(() => null);
  if (!res.ok) {
    // The session cookie lasts 90 days, but a roster removal or a rotated
    // signing key ends a session at once, and only a call finds that out.
    // GET /session answers 200 either way, so a 401 is always this.
    if (res.status === 401) signedOut();
    throw new ApiError(res.status, payload?.error ?? `${method} ${path} failed (${res.status})`);
  }
  return payload;
}

const get = (path) => request("GET", path);

/** Everything the public landing page needs, in one round trip. */
export const fetchOverview = (days) =>
  get(`/public/overview${days ? `?days=${days}` : ""}`);

/** @returns {Promise<{user: object|null, dev: boolean, providers: string[]}>} */
export const fetchSession = () => get("/session");

/**
 * Sign in as anyone on the roster, by address or by role. This is the
 * development sign-in: a deployed server doesn't register the route, and the
 * page uses /auth/{provider}/start instead.
 */
export const createSession = (body) => request("POST", "/session", body);
export const deleteSession = () => request("DELETE", "/session");

/** Sign in with an address and password. A wrong one is an ApiError like any other. */
export const passwordSignIn = (body) => request("POST", "/auth/password", body);
/** Ask for a link to set a password. The answer is the same whoever is asking. */
export const requestPasswordLink = (email) => request("POST", "/auth/password/link", { email });
/** Whose an emailed link is: {user, first}, or an ApiError if it's no longer good. */
export const fetchPasswordLink = (token) =>
  get(`/auth/password/link?${new URLSearchParams({ token })}`);
/** Spend a link on a new password, which also signs this browser in. */
export const setPassword = (body) => request("PUT", "/auth/password", body);

/** Everyone on the roster, for the sign-in picker. Only exists in dev mode. */
export const fetchDevPeople = () => get("/dev/people");

export const fetchStations = () => get("/stations");

/** The signed-in volunteer's own cards, newest first. */
export const fetchMyUploads = () => get("/uploads");
/** One of the volunteer's cards, and each file on its list with its status ({path, bytes, recordedAt, status}). */
export const fetchUpload = (reference) => get(`/uploads/${encodeURIComponent(reference)}`);

/**
 * Register a card that is about to be sent, with the files read off it
 * ({path, bytes}). Returns the card with its reference, and each file
 * with its status on the server, so a resume sends only what's missing.
 */
export const createUpload = (body) => request("POST", "/uploads", body);

/**
 * Say the transfer is running ("in_progress") or has stopped ("interrupted").
 * The files themselves go over tus (upload-flow.js), and the server counts them.
 */
export const reportProgress = (reference, body) =>
  request("POST", `/uploads/${encodeURIComponent(reference)}/progress`, body);

/**
 * Send a body for the server to read and throw away (the upload speed test,
 * speed-test.js). Not JSON going out, so it doesn't go through request; a 401
 * still signs the app out the same way.
 */
export async function sendSpeedTest(body, signal) {
  const res = await fetch(`${BASE}/speedtest`, { method: "POST", body, signal });
  if (!res.ok) {
    if (res.status === 401) signedOut();
    const payload = await res.json().catch(() => null);
    throw new ApiError(res.status, payload?.error ?? `POST /speedtest failed (${res.status})`);
  }
}

// Detections are anyone's to hear and review once signed in, on every card.

/**
 * Every card's detections, a page at a time, with the species among them:
 * {detections, total, species, window}. params are the API's, all optional:
 * since, until, status, minConfidence, species, sort, order, limit, offset.
 * With no since the server answers for a window of recent days rather than
 * reading every detection there is, and window says which.
 */
export const fetchDetections = (params) => {
  const search = new URLSearchParams(params).toString();
  return get(`/detections${search ? `?${search}` : ""}`);
};
/**
 * Every detection fetchDetections' params match, not a page of them, as a
 * JSON file to save: a Blob of {exportedAt, ...what was asked, total,
 * detections}. limit and offset don't apply. A file, not JSON to parse, so it
 * doesn't go through request; a 401 still signs the app out the same way.
 */
export async function exportDetections(params) {
  const search = new URLSearchParams(params).toString();
  const res = await fetch(`${BASE}/detections/export${search ? `?${search}` : ""}`);
  if (!res.ok) {
    if (res.status === 401) signedOut();
    const payload = await res.json().catch(() => null);
    throw new ApiError(res.status, payload?.error ?? `GET /detections/export failed (${res.status})`);
  }
  return res.blob();
}
/**
 * What BirdNET heard in one file of a card, in the order it was heard, at most
 * limit of them: {detections, total}. total is how many there are in all.
 * Without a limit the server's own page size applies -- a caller that wants a
 * whole file's detections has to ask for them. model "perch" asks for what
 * Perch heard instead.
 */
export const fetchFileDetections = (reference, fileId, limit, model) => {
  const search = new URLSearchParams({ file: fileId });
  if (limit) search.set("limit", limit);
  if (model && model !== "birdnet") search.set("model", model);
  return get(`/detections/${encodeURIComponent(reference)}?${search}`);
};
/** One detection, with the card and the file it was heard in. */
export const fetchDetection = (reference, id) =>
  get(`/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}`);
/** Where a detection's clip plays from: a FLAC, a few seconds either side of what was heard. */
export const clipURL = (reference, id) =>
  `${BASE}/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}/clip`;
/**
 * Override a detection's verdict: "confirmed", "rejected", or "unreviewed" to
 * undo the override -- which leaves it where its bout's verdict or the review
 * rules put it, so the detection that comes back may say "accepted". A
 * birding expert's or admin's.
 */
export const reviewDetection = (reference, id, status) =>
  request("PUT", `/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}/review`, { status });

// The review rules (REVIEW.md): which species are accepted on BirdNET's word,
// and which a reviewer hears, each above a confidence floor of its own.

/** {rules, suggested}: rules is null until they are first saved; suggested is the defaults a first table starts from. */
export const fetchReviewRules = () => get("/review/rules");
/**
 * Replace the rules: {version, defaults, boutGapMinutes, spotChecksPerSpecies,
 * species}, version being the one they were read at (0 for the first save).
 * A 409 means someone else saved them meanwhile. Admins and birding experts only.
 */
export const saveReviewRules = (rules) => request("PUT", "/review/rules", rules);
/** What BirdNET heard in the last `days` days, species by species, in confidence bands: {since, until, species}. */
export const fetchSpeciesHeard = (days) => get(`/review/species${days ? `?days=${days}` : ""}`);

// Reviewing bouts (REVIEW.md): a bout is one species heard on one card, and
// a verdict on its best clips stands for every detection in it.

/** {cards}: every card with bouts, newest first, each with how many are settled and disputed. */
export const fetchReviewCards = () => get("/review/cards");
/**
 * {card, bouts, left}: a card's bouts in the order heard, and how many wait
 * for you. A bout's other verdicts are left out until you give yours.
 */
export const fetchBouts = (reference) => get(`/review/cards/${encodeURIComponent(reference)}/bouts`);
/** {bout, detections}: one bout, and its detections in the order heard. */
export const fetchBout = (reference, id) =>
  get(`/review/cards/${encodeURIComponent(reference)}/bouts/${encodeURIComponent(id)}`);
/**
 * Give your verdict on a bout, or change it: {verdict: "confirm" | "reject" |
 * "unsure"}, and scientificName and commonName to confirm another species.
 * Answers {bout}, with everyone's verdicts. A 409 means it settled meanwhile.
 */
export const giveVerdict = (reference, id, body) =>
  request("PUT", `/review/cards/${encodeURIComponent(reference)}/bouts/${encodeURIComponent(id)}/verdict`, body);

/** {disagreements}: every card's disputed bouts, each {card, bout}. A birding expert's. */
export const fetchDisagreements = () => get("/review/disagreements");
/**
 * A birding expert's final say on a bout: {verdict: "confirm" | "reject"},
 * and scientificName and commonName to confirm another species. It settles
 * a disputed bout, or overrides how its listeners settled it. Answers {bout}.
 */
export const resolveBout = (reference, id, body) =>
  request("PUT", `/review/cards/${encodeURIComponent(reference)}/bouts/${encodeURIComponent(id)}/resolution`, body);

// Connecting an LLM tool over MCP: the consent page's two calls. signed is
// the authorization request the server signed into the page's URL.

/** Which app is asking to connect, and where it sends them back: {client, redirectHost, user}. */
export const fetchConnectRequest = (signed) =>
  get(`/oauth/request?${new URLSearchParams({ request: signed })}`);
/** Allow or refuse it. Returns {redirect}: where to send the browser, back to the app. */
export const decideConnectRequest = (signed, approve) =>
  request("POST", "/oauth/authorize", { request: signed, approve });

export const fetchAllUploads = () => get("/admin/uploads");
/** One card and every file on it, with where each is in upload and analysis. */
export const fetchCardFiles = (reference) => get(`/admin/uploads/${encodeURIComponent(reference)}`);
/** Delete a card for good: its audio, its files and every detection in them. */
export const deleteUpload = (reference) =>
  request("DELETE", `/admin/uploads/${encodeURIComponent(reference)}`);
export const fetchPeople = () => get("/admin/people");
export const addPerson = (body) => request("POST", "/admin/people", body);
/** Replace someone's name, email, role (admin or volunteer) and roles (roles.js). */
export const updatePerson = (id, body) =>
  request("PUT", `/admin/people/${encodeURIComponent(id)}`, body);
/** Take someone off the roster. Refused for yourself and for the last admin. */
export const removePerson = (id) => request("DELETE", `/admin/people/${encodeURIComponent(id)}`);
export const addStation = (body) => request("POST", "/admin/stations", body);
/** Rename or move a recorder. Its id is printed on the unit and can't change. */
export const updateStation = (id, body) =>
  request("PUT", `/admin/stations/${encodeURIComponent(id)}`, body);
export const removeStation = (id) => request("DELETE", `/admin/stations/${encodeURIComponent(id)}`);
