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
    throw new ApiError(res.status, payload?.error ?? `${method} ${path} failed (${res.status})`);
  }
  return payload;
}

const get = (path) => request("GET", path);

/** Everything the public landing page needs, in one round trip. */
export const fetchOverview = (days) =>
  get(`/public/overview${days ? `?days=${days}` : ""}`);

/** @returns {Promise<{user: object|null, dev: boolean}>} */
export const fetchSession = () => get("/session");

/**
 * Sign in. Pass an email address, or -- until an identity provider is wired
 * up -- a role to be signed in as the first person on the roster holding it.
 */
export const createSession = (body) => request("POST", "/session", body);
export const deleteSession = () => request("DELETE", "/session");

/** Everyone on the roster, for the sign-in picker. Only exists in dev mode. */
export const fetchDevPeople = () => get("/dev/people");

export const fetchStations = () => get("/stations");

/** The signed-in volunteer's own cards, newest first. */
export const fetchMyUploads = () => get("/uploads");
export const fetchUpload = (reference) => get(`/uploads/${encodeURIComponent(reference)}`);

/** Register a card that is about to be sent. Returns it with its reference. */
export const createUpload = (body) => request("POST", "/uploads", body);

/** Report what has landed so far. The server only ever moves the count up. */
export const reportProgress = (reference, body) =>
  request("POST", `/uploads/${encodeURIComponent(reference)}/progress`, body);

export const fetchAllUploads = () => get("/admin/uploads");
export const fetchPeople = () => get("/admin/people");
export const addPerson = (body) => request("POST", "/admin/people", body);
/** Replace someone's name, email and role. */
export const updatePerson = (id, body) =>
  request("PUT", `/admin/people/${encodeURIComponent(id)}`, body);
/** Take someone off the roster. Refused for yourself and for the last admin. */
export const removePerson = (id) => request("DELETE", `/admin/people/${encodeURIComponent(id)}`);
export const addStation = (body) => request("POST", "/admin/stations", body);
