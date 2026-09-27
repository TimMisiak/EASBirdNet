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

/** Everyone on the roster, for the sign-in picker. Only exists in dev mode. */
export const fetchDevPeople = () => get("/dev/people");

export const fetchStations = () => get("/stations");

/** The signed-in volunteer's own cards, newest first. */
export const fetchMyUploads = () => get("/uploads");
/** One of the volunteer's cards, and each file on its list with its status ({path, bytes, night, status}). */
export const fetchUpload = (reference) => get(`/uploads/${encodeURIComponent(reference)}`);

/**
 * Register a card that is about to be sent, with the files read off it
 * ({path, bytes, night}). Returns the card with its reference, and each file
 * with its status on the server, so a resume sends only what's missing.
 */
export const createUpload = (body) => request("POST", "/uploads", body);

/**
 * Say the transfer is running ("in_progress") or has stopped ("interrupted").
 * The files themselves go over tus (upload-flow.js), and the server counts them.
 */
export const reportProgress = (reference, body) =>
  request("POST", `/uploads/${encodeURIComponent(reference)}/progress`, body);

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
 * What BirdNET heard in one file of a card, in the order it was heard, at most
 * limit of them: {detections, total}. total is how many there are in all.
 * Without a limit the server's own page size applies -- a caller that wants a
 * whole file's detections has to ask for them.
 */
export const fetchFileDetections = (reference, fileId, limit) => {
  const search = new URLSearchParams({ file: fileId });
  if (limit) search.set("limit", limit);
  return get(`/detections/${encodeURIComponent(reference)}?${search}`);
};
/** One detection, with the card and the file it was heard in. */
export const fetchDetection = (reference, id) =>
  get(`/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}`);
/** Where a detection's clip plays from: a FLAC, a few seconds either side of what was heard. */
export const clipURL = (reference, id) =>
  `${BASE}/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}/clip`;
/** Record a verdict on a detection: "confirmed", "rejected", or back to "unreviewed". */
export const reviewDetection = (reference, id, status) =>
  request("PUT", `/detections/${encodeURIComponent(reference)}/${encodeURIComponent(id)}/review`, { status });

export const fetchAllUploads = () => get("/admin/uploads");
/** One card and every file on it, with where each is in upload and analysis. */
export const fetchCardFiles = (reference) => get(`/admin/uploads/${encodeURIComponent(reference)}`);
/** Delete a card for good: its audio, its files and every detection in them. */
export const deleteUpload = (reference) =>
  request("DELETE", `/admin/uploads/${encodeURIComponent(reference)}`);
export const fetchPeople = () => get("/admin/people");
export const addPerson = (body) => request("POST", "/admin/people", body);
/** Replace someone's name, email and role. */
export const updatePerson = (id, body) =>
  request("PUT", `/admin/people/${encodeURIComponent(id)}`, body);
/** Take someone off the roster. Refused for yourself and for the last admin. */
export const removePerson = (id) => request("DELETE", `/admin/people/${encodeURIComponent(id)}`);
export const addStation = (body) => request("POST", "/admin/stations", body);
/** Rename or move a recorder. Its id is printed on the unit and can't change. */
export const updateStation = (id, body) =>
  request("PUT", `/admin/stations/${encodeURIComponent(id)}`, body);
export const removeStation = (id) => request("DELETE", `/admin/stations/${encodeURIComponent(id)}`);
