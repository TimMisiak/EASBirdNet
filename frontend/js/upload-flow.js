// The SD-card upload, from "which station is this" to "the card is safe to
// erase". It spans four routes, so the state lives here rather than in any one
// page component, and the pages subscribe to it.
//
// Files go one at a time, each as its own tus upload (https://tus.io) to
// /api/v1/tus/, in chunks: a dropped connection or a pause picks a file up from
// its last chunk rather than its first byte. The server checks every file
// against the list the card was registered with, and counts it once its last
// byte lands, so the card's counts always come from the server. This module
// tracks the files in this tab and the one that's moving.

import * as api from "./api.js";
import { isUnfinished } from "./upload-status.js";

/**
 * tus-js-client, from jsDelivr, pinned and hash-checked. This is its browser
 * build, a classic script that defines window.tus: the package's ES modules
 * import CommonJS dependencies, and jsDelivr's generated +esm bundles can't be
 * hash-checked. It loads when an upload starts, not with every page.
 */
const TUS_CLIENT = {
  src: "https://cdn.jsdelivr.net/npm/tus-js-client@4.3.1/dist/tus.min.js",
  integrity: "sha384-UlHjK3F7TCQCEUpnoa1ohMbP2oaWB3Aypv4gMo511vaZ86uUZ0Zv7UzZ0J1zRUT1",
};

const ENDPOINT = "/api/v1/tus/";
/**
 * The most one request carries. On a slow home uplink a chunk still lands well
 * inside a proxy's request timeout (240 s at the Azure Container Apps ingress),
 * and it bounds what the server holds per request on its way to blob storage.
 */
const CHUNK_BYTES = 50_000_000;
/** Waits before each retry of a failed request. After the last, the card is interrupted. */
const RETRY_DELAYS = [0, 1_000, 3_000, 5_000, 10_000, 20_000];
/** Megabits per second assumed for estimates, until there is a real speed. */
const ASSUMED_MBPS = 110;
const ASSUMED_BYTES_PER_SECOND = (ASSUMED_MBPS * 1e6) / 8;
/** The speed is averaged over this long, so a stall shows within seconds. */
const SPEED_WINDOW_MS = 15_000;
/** Progress arrives many times a second; pages hear about it this often. */
const NOTIFY_EVERY_MS = 150;
/** How the server turns down one file (see beforeFileUpload); the others can still go. */
const FILE_REFUSED = [400, 413, 422];

const KEY = "birdsense.upload.reference";

const listeners = new Set();
let state = initial();

// The file being sent, and how fast things are moving. None of it is rendered
// directly, so it stays out of state.
let inFlight = null;
let samples = [];
let sentThisTab = 0;
let elapsedMs = 0;
let runningSince = 0;
let lastNotify = 0;
let tusClient = null;

function initial() {
  return {
    /** "idle" | "ready" | "uploading" | "paused" | "interrupted" | "done" */
    status: "idle",
    stationId: "",
    pulledOn: today(),
    notes: "",
    card: null,
    upload: null,
    /**
     * The card's files in the order they are sent, once the card has been read
     * in this tab: {path, bytes, night, file, state, sent, error}. state is
     * "waiting", "sending", "done", "already" (the server had it before this
     * tab sent anything) or "failed".
     */
    files: [],
    error: null,
  };
}

export const get = () => state;

export function subscribe(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function set(patch) {
  state = { ...state, ...patch };
  lastNotify = Date.now();
  for (const listener of listeners) listener(state);
}

/** Step 1 keeps the form in the flow so going back and forth doesn't lose it. */
export const setDetails = (details) => set(details);

/**
 * Step 1 → 2: register the scanned card with the server. Re-registering the
 * same card is how a resume starts, so the server answers with the files it
 * already has, and only the rest are queued.
 */
export async function registerCard(card) {
  const { upload, files } = await api.createUpload(registration(card));
  sessionStorage.setItem(KEY, upload.reference);
  const stored = storedPaths(files);
  set({
    card,
    upload,
    status: "ready",
    error: null,
    files: card.files.map((f) =>
      stored.has(f.path)
        ? { ...f, state: "already", sent: f.bytes, error: null }
        : { ...f, state: "waiting", sent: 0, error: null },
    ),
  });
  return upload;
}

function registration(card) {
  return {
    stationId: state.stationId,
    pulledOn: state.pulledOn,
    notes: state.notes,
    nights: card.nights.map(({ date, files, bytes, flag }) => ({
      date,
      files,
      bytes,
      ...(flag ? { flag } : {}),
    })),
    files: card.files.map(({ path, bytes, night }) => ({ path, bytes, night })),
  };
}

function storedPaths(files) {
  return new Set(
    files.filter((f) => f.status === "uploaded" || f.status === "analyzed").map((f) => f.path),
  );
}

/**
 * Pick up a card that is already on the server -- "Resume upload" from the
 * volunteer's home, or a reload part-way through the wizard. A reload loses
 * the card's files, so unless this tab is already sending the card, the
 * volunteer chooses the card again to carry on.
 */
export async function adopt(reference) {
  if (state.upload?.reference === reference) return state.upload;
  const { upload } = await api.fetchUpload(reference);
  stop();
  sessionStorage.setItem(KEY, reference);
  set({
    ...initial(),
    upload,
    stationId: upload.stationId,
    pulledOn: upload.pulledOn,
    notes: upload.notes,
    status: "interrupted",
  });
  return upload;
}

/** The card the pages should show, after a reload if need be. */
export async function current() {
  if (state.upload) return state.upload;
  const reference = sessionStorage.getItem(KEY);
  return reference ? adopt(reference) : null;
}

/** Send what's still to go -- including files that failed, which get another try. */
export function start() {
  if (!state.upload || !state.files.length || state.status === "uploading") return;
  for (const f of state.files) {
    if (f.state === "failed") Object.assign(f, { state: "waiting", error: null });
  }
  set({ status: "uploading", error: null });
  run(state.upload.reference);
}

export function pause() {
  if (state.status !== "uploading") return;
  stop();
  set({ status: "paused" });
  report(state.upload.reference, "in_progress");
}

export function reset() {
  stop();
  elapsedMs = 0;
  sessionStorage.removeItem(KEY);
  state = initial();
  for (const listener of listeners) listener(state);
}

async function run(reference) {
  runningSince = Date.now();
  window.addEventListener("beforeunload", holdPage);
  try {
    const tus = await loadTusClient();
    report(reference, "in_progress");
    let resynced = false;
    for (;;) {
      if (!(await sendWaiting(tus, reference))) return;
      const { upload } = await api.fetchUpload(reference);
      if (!running(reference)) return;
      if (!isUnfinished(upload)) {
        stop();
        set({ upload, status: "done" });
        return;
      }
      set({ upload });
      const failed = state.files.filter((f) => f.state === "failed").length;
      if (failed) {
        throw new Error(
          `${failed === 1 ? "One file" : `${failed} files`} couldn't be uploaded -- the list says why. ` +
            "Try again, or choose the card again if it has changed.",
        );
      }
      if (resynced) {
        throw new Error("Every file was sent, but we haven't counted them all. Try again to resend the missing ones.");
      }
      // Every file went, but the card isn't in: a file's last request failed
      // after it was stored. Ask which files the server has, and send the rest.
      await resync();
      resynced = true;
    }
  } catch (error) {
    if (running(reference)) interrupt(reference, error);
  } finally {
    if (state.status !== "uploading") window.removeEventListener("beforeunload", holdPage);
  }
}

const running = (reference) => state.upload?.reference === reference && state.status === "uploading";

/** Send every waiting file in order. False if the run was stopped part-way. */
async function sendWaiting(tus, reference) {
  for (;;) {
    if (!running(reference)) return false;
    const entry = state.files.find((f) => f.state === "waiting");
    if (!entry) return true;
    try {
      await send(tus, reference, entry);
      Object.assign(entry, { state: "done", sent: entry.bytes });
    } catch (error) {
      if (error instanceof Stopped) return false;
      const status = error?.originalResponse?.getStatus?.();
      if (!FILE_REFUSED.includes(status)) throw explain(status);
      const refusal = serverReason(error);
      if (refusal?.code === "ERR_FILE_RECEIVED") {
        Object.assign(entry, { state: "already", sent: entry.bytes });
      } else {
        Object.assign(entry, { state: "failed", sent: 0, error: refusal?.message ?? error.message });
      }
    }
    set({ files: state.files });
  }
}

class Stopped extends Error {}

function send(tus, reference, entry) {
  return new Promise((resolve, reject) => {
    let stopped = false;
    // The first progress report of a resumed file is what had already landed.
    let first = true;
    const upload = new tus.Upload(entry.file, {
      endpoint: ENDPOINT,
      chunkSize: CHUNK_BYTES,
      retryDelays: RETRY_DELAYS,
      metadata: { reference, path: entry.path },
      // Resume by card and path: a recorder names its files the same way on
      // every card, so the file name alone would find another card's upload.
      fingerprint: async () => `birdsense:${reference}:${entry.path}:${entry.bytes}:${entry.file.lastModified}`,
      removeFingerprintOnSuccess: true,
      onProgress: (sent) => {
        progress(entry, sent, first);
        first = false;
      },
      onSuccess: resolve,
      onError: reject,
    });
    inFlight = {
      stop() {
        stopped = true;
        upload.abort();
        entry.state = "waiting";
        reject(new Stopped());
      },
    };
    entry.state = "sending";
    set({ files: state.files });
    upload.findPreviousUploads().then((previous) => {
      if (stopped) return;
      if (previous.length) upload.resumeFromPreviousUpload(previous[0]);
      upload.start();
    }, reject);
  }).finally(() => {
    inFlight = null;
  });
}

function progress(entry, sent, first) {
  if (!first && sent > entry.sent) sentThisTab += sent - entry.sent;
  entry.sent = sent;
  const now = Date.now();
  samples.push([now, sentThisTab]);
  while (samples.length > 2 && now - samples[1][0] >= SPEED_WINDOW_MS) samples.shift();
  if (now - lastNotify >= NOTIFY_EVERY_MS) set({ files: state.files });
}

/** Register the card again to learn which files the server has, and queue the rest. */
async function resync() {
  const { upload, files } = await api.createUpload(registration(state.card));
  const stored = storedPaths(files);
  for (const f of state.files) {
    if (!stored.has(f.path)) Object.assign(f, { state: "waiting", sent: 0, error: null });
  }
  set({ upload, files: state.files });
}

function interrupt(reference, error) {
  stop();
  set({ status: "interrupted", error });
  report(reference, "interrupted");
}

/** Stop the file in flight and the clock. */
function stop() {
  inFlight?.stop();
  if (runningSince) elapsedMs += Date.now() - runningSince;
  runningSince = 0;
  samples = [];
  window.removeEventListener("beforeunload", holdPage);
}

/** What the volunteer is told when the whole card stops. */
function explain(status) {
  if (status === 401) {
    return new Error("You've been signed out. Sign in again, then choose the card again to carry on.");
  }
  if (status >= 500) {
    return new Error("Something went wrong on our side while receiving a file. Try again in a few minutes.");
  }
  // Otherwise it's the connection or the card, and the page lists the usual causes.
  return new Error("The connection dropped, or the card couldn't be read.");
}

/** tusd's refusals read "ERR_CODE: message". */
function serverReason(error) {
  const match = /^(ERR_[A-Z_]+): (.+)$/m.exec(error?.originalResponse?.getBody?.() ?? "");
  return match && { code: match[1], message: match[2] };
}

async function report(reference, status) {
  try {
    const { upload } = await api.reportProgress(reference, { status });
    if (state.upload?.reference === reference && state.status !== "done") set({ upload });
  } catch {
    // Only the chip on the volunteer's home page reads this. The files have
    // their own retries, and are what matters.
  }
}

function loadTusClient() {
  tusClient ??= new Promise((resolve, reject) => {
    const script = Object.assign(document.createElement("script"), {
      src: TUS_CLIENT.src,
      integrity: TUS_CLIENT.integrity,
      crossOrigin: "anonymous",
    });
    script.onload = () =>
      window.tus?.isSupported
        ? resolve(window.tus)
        : reject(new Error("This browser can't upload the card. Try a current Chrome, Edge, Firefox or Safari."));
    script.onerror = () => {
      script.remove();
      tusClient = null;
      reject(new Error("The uploader didn't load. Check the connection, then try again."));
    };
    document.head.append(script);
  });
  return tusClient;
}

/** While a card is moving, closing the tab asks first. */
function holdPage(event) {
  event.preventDefault();
  event.returnValue = "";
}

/** Files and bytes in, counting the file in flight by what has landed of it. */
export function totals() {
  const { upload, files } = state;
  if (!files.length) return { files: upload?.filesUploaded ?? 0, bytes: upload?.bytesUploaded ?? 0 };
  let count = 0;
  let bytes = 0;
  for (const f of files) {
    if (f.state === "done" || f.state === "already") count += 1;
    bytes += f.sent;
  }
  return { files: count, bytes };
}

/** Bytes per second over the last few seconds, or null until there's enough to say. */
export function bytesPerSecond() {
  if (samples.length < 2) return null;
  const [t0, b0] = samples[0];
  const [t1, b1] = samples[samples.length - 1];
  return t1 - t0 >= 2_000 ? ((b1 - b0) * 1_000) / (t1 - t0) : null;
}

/** Minutes of transfer left, at the measured speed once there is one. */
export const minutesRemaining = () =>
  ((state.upload?.totalBytes ?? 0) - totals().bytes) / ((bytesPerSecond() ?? ASSUMED_BYTES_PER_SECOND) * 60);
export const minutesElapsed = () => (elapsedMs + (runningSince ? Date.now() - runningSince : 0)) / 60_000;
/** Minutes to send this many bytes at the assumed speed, before anything has been sent. */
export const totalMinutes = (bytes) => bytes / (ASSUMED_BYTES_PER_SECOND * 60);
export const assumedSpeed = () => `${ASSUMED_MBPS} Mb/s`;

function today() {
  return new Date().toISOString().slice(0, 10);
}
