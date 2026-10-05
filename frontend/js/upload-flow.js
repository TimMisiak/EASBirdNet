// The SD-card upload, from "which station is this" to "the card is safe to
// erase". It spans four routes, so the state lives here rather than in any one
// page component, and the pages subscribe to it.
//
// Each file is its own tus upload (https://tus.io) to /api/v1/tus/, sent in
// chunks: a dropped connection or a pause picks a file up from its last chunk
// rather than its first byte. On a fast line a few files go at once, because
// the server answers each chunk only once it has passed it on to storage, and
// one file at a time leaves the line idle while it does (PARALLEL-UPLOADS.md).
// The server checks every file against the list the card was registered with,
// and counts it once its last byte lands, so the card's counts always come
// from the server. This module tracks the files in this tab and the ones that
// are moving.

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
/**
 * The most files that go at once. Each waits on the server between its chunks
 * -- measured on Azure, ~2 s to stage every 50 MB -- and the others keep the
 * line busy meanwhile: on a 518 Mbit/s line, 3 at once reached 260 Mbit/s and
 * 6 reached 460 (PARALLEL-UPLOADS.md, *Measured on Azure*). A browser can set
 * its own for measuring (localStorage "birdsense.upload.parallelFiles", 1-8).
 */
export const PARALLEL_FILES = parallelOverride() ?? 6;

function parallelOverride() {
  try {
    const n = Number(localStorage.getItem("birdsense.upload.parallelFiles"));
    return Number.isInteger(n) && n >= 1 && n <= 8 ? n : null;
  } catch {
    return null;
  }
}
/**
 * The share of the line each file gets: one file at a time per this much
 * measured speed. Splitting a line thinner only stretches each chunk toward
 * the ingress timeout, so a line under twice this goes one file at a time, and
 * a 50 MB chunk arrives in about 20 s at any number of files.
 */
const MBPS_PER_FILE = 20;
/** How the server turns down one file (see beforeFileUpload); the others can still go. */
const FILE_REFUSED = [400, 413, 422];

const KEY = "birdsense.upload.reference";

const listeners = new Set();
let state = initial();

// The files being sent, and how fast things are moving. None of it is rendered
// directly, so it stays out of state.
const inFlight = new Set();
/**
 * The files being sent now: `fill` starts more when there's room for them.
 * Stopping clears it, so what a stopped run still has in hand can't start
 * files beside the next run's.
 */
let sending = null;
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
    /**
     * True when the card was picked up from the server ("Resume upload" on
     * the volunteer's cards, or a reload) rather than started in this tab. Its
     * station and date are what make it that card, so step 1 only asks for the
     * card again.
     */
    resuming: false,
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
    // The same card chosen again after it was received: nothing left to send.
    status: isUnfinished(upload) ? "ready" : "done",
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

const isStored = (f) => f.status === "uploaded" || f.status === "analyzed";

function storedPaths(files) {
  return new Set(files.filter(isStored).map((f) => f.path));
}

/**
 * Pick up a card that is already on the server -- "Resume upload" on the
 * volunteer's cards, or a reload part-way through the wizard. A reload loses
 * the card's files, so unless this tab is already sending the card, the
 * volunteer chooses the card again to carry on.
 */
export async function adopt(reference) {
  if (state.upload?.reference === reference) return state.upload;
  const { upload } = await api.fetchUpload(reference);
  // Picking up another card stops the one this tab is sending. Its files so
  // far are safe, and a file part-way across resumes when it's chosen again.
  const sending = state.status === "uploading" ? state.upload.reference : null;
  stop();
  if (sending) report(sending, "interrupted");
  sessionStorage.setItem(KEY, reference);
  const unfinished = isUnfinished(upload);
  set({
    ...initial(),
    upload,
    stationId: upload.stationId,
    pulledOn: upload.pulledOn,
    notes: upload.notes,
    resuming: unfinished,
    status: unfinished ? "interrupted" : "done",
  });
  return upload;
}

/**
 * "Resume upload": pick a card up, and say where its upload carries on. A card
 * this tab is already sending goes on where it is; any other needs the card
 * chosen again at step 1, so the server can say what's missing.
 */
export async function resume(reference) {
  await adopt(reference);
  return state.files.length ? "/app/upload/progress" : "/app/upload";
}

/** The card the pages should show, after a reload if need be. */
export async function current() {
  if (state.upload) return state.upload;
  const reference = sessionStorage.getItem(KEY);
  if (!reference) return null;
  try {
    return await adopt(reference);
  } catch (error) {
    // A card that's gone, or isn't this volunteer's, is nothing to pick up.
    if (error.status !== 404) throw error;
    sessionStorage.removeItem(KEY);
    return null;
  }
}

/** The card step 1 is finishing, or null when it's a new one. */
export const resumingCard = () =>
  state.resuming && state.upload && isUnfinished(state.upload) ? state.upload : null;

/**
 * The files the server already has for the card being finished that aren't on
 * a freshly read card at the same path and size. Registering that card would
 * take them off the card's list, so step 1 asks first. The usual cause is
 * choosing a folder on the card instead of the card, which changes every path.
 */
export async function storedFilesMissingFrom(card) {
  const { files } = await api.fetchUpload(state.upload.reference);
  const onCard = new Map(card.files.map((f) => [f.path, f.bytes]));
  return files.filter((f) => isStored(f) && onCard.get(f.path) !== f.bytes);
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

/**
 * Send every waiting file, in order, as many at once as the line warrants
 * (filesAtOnce). True once every file is sent or turned down; false if the run
 * was stopped part-way. A failure that isn't one file being turned down stops
 * the lot, as it did one file at a time.
 */
function sendWaiting(tus, reference) {
  return new Promise((resolve, reject) => {
    // Files only leave "waiting" during a run, so the next one is always at
    // or after the last one taken, and finding it never rescans the card.
    let next = 0;
    let settled = false;
    const run = {};
    const settle = (done, value) => {
      if (settled) return;
      settled = true;
      if (sending === run) sending = null;
      done(value);
    };
    const nextWaiting = () => {
      while (next < state.files.length && state.files[next].state !== "waiting") next += 1;
      return state.files[next];
    };
    // This run's own, so a file stopped by a pause settles this run, not
    // whichever a quick resume has started since.
    const fill = () => {
      if (settled) return;
      if (sending !== run || !running(reference)) return settle(resolve, false);
      while (inFlight.size < filesAtOnce()) {
        const entry = nextWaiting();
        if (!entry) break;
        sendOne(tus, reference, entry).then(fill, (error) => settle(reject, error));
      }
      if (!inFlight.size && !nextWaiting()) settle(resolve, true);
    };
    run.fill = fill;
    sending = run;
    fill();
  });
}

/** One file, to the end: sent, already in, or turned down. Throws what should stop the card. */
async function sendOne(tus, reference, entry) {
  try {
    await send(tus, reference, entry);
    Object.assign(entry, { state: "done", sent: entry.bytes });
  } catch (error) {
    if (error instanceof Stopped) return;
    const status = error?.originalResponse?.getStatus?.();
    if (!FILE_REFUSED.includes(status)) {
      // Not this file's fault: it goes again from its last chunk next time.
      entry.state = "waiting";
      throw explain(status);
    }
    const refusal = serverReason(error);
    if (refusal?.code === "ERR_FILE_RECEIVED") {
      Object.assign(entry, { state: "already", sent: entry.bytes });
    } else {
      Object.assign(entry, { state: "failed", sent: 0, error: refusal?.message ?? error.message });
    }
  }
  if (running(reference)) set({ files: state.files });
}

/** One file until the line is measured, then one per MBPS_PER_FILE of it, up to PARALLEL_FILES. */
function filesAtOnce() {
  const speed = bytesPerSecond();
  if (speed === null) return 1;
  return Math.min(PARALLEL_FILES, Math.max(1, Math.floor((speed * 8) / 1e6 / MBPS_PER_FILE)));
}

class Stopped extends Error {}

function send(tus, reference, entry) {
  let handle = null;
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
    handle = {
      stop() {
        stopped = true;
        upload.abort();
        entry.state = "waiting";
        reject(new Stopped());
      },
    };
    inFlight.add(handle);
    entry.state = "sending";
    set({ files: state.files });
    upload.findPreviousUploads().then((previous) => {
      if (stopped) return;
      if (previous.length) upload.resumeFromPreviousUpload(previous[0]);
      upload.start();
    }, reject);
  }).finally(() => {
    inFlight.delete(handle);
  });
}

function progress(entry, sent, first) {
  if (!first && sent > entry.sent) sentThisTab += sent - entry.sent;
  entry.sent = sent;
  const now = Date.now();
  samples.push([now, sentThisTab]);
  while (samples.length > 2 && now - samples[1][0] >= SPEED_WINDOW_MS) samples.shift();
  if (now - lastNotify >= NOTIFY_EVERY_MS) set({ files: state.files });
  // Once the line is measured fast, the other files can start.
  sending?.fill();
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

/** Stop the files in flight and the clock. */
function stop() {
  sending = null;
  for (const upload of [...inFlight]) upload.stop();
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
    // Only the chip on the volunteer's list of cards reads this. The files have
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

/** Today's date where the volunteer is. toISOString's is UTC's, a day ahead every evening in Pacific time. */
export function today() {
  const d = new Date();
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
