// Thin wrapper over the JSON API. Components call these functions instead of
// fetch() directly, so error handling and the /api/v1 prefix live in one place.

const BASE = "/api/v1";

async function getJSON(path) {
  const res = await fetch(`${BASE}${path}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`${path} failed: ${res.status} ${res.statusText}`);
  }
  return res.json();
}

/** @returns {Promise<Array<object>>} recent detections, newest first. */
export async function fetchDetections() {
  const { detections } = await getJSON("/detections");
  return detections ?? [];
}
