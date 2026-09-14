// The SD-card upload, from "which station is this" to "the card is safe to
// erase". It spans four routes, so the state lives here rather than in any one
// page component, and the pages subscribe to it.
//
// PLACEHOLDER TRANSFER: there is no storage backend to send 128 GB to yet, so
// the transfer itself is simulated -- a clock advances the byte count and the
// browser reports progress to /api/v1/uploads/{ref}/progress, which is what the
// real uploader will do per batch. Everything the volunteer reads (sizes,
// estimates, the manifest) comes from the actual card, and the estimates are
// computed at the real link speed, so only the waiting is fake.

import * as api from "./api.js";

/** Megabits per second. Measured for real once there is something to measure. */
const LINK_MBPS = 110;
const BYTES_PER_MINUTE = (LINK_MBPS * 1e6 * 60) / 8;

/** How much faster than real time the placeholder transfer runs. */
const DEMO_SPEEDUP = 360;
const TICK_MS = 200;
/** Don't tell the server about every tick; a batch every couple of seconds. */
const REPORT_EVERY_MS = 2000;

const KEY = "birdsense.upload.reference";

const listeners = new Set();
let timer = null;
let lastReport = 0;

let state = {
  /** "idle" | "scanning" | "ready" | "uploading" | "paused" | "interrupted" | "done" */
  status: "idle",
  stationId: "",
  pulledOn: today(),
  notes: "",
  card: null,
  upload: null,
  filesUploaded: 0,
  bytesUploaded: 0,
  error: null,
};

export const get = () => state;

export function subscribe(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function set(patch) {
  state = { ...state, ...patch };
  for (const listener of listeners) listener(state);
}

/** Step 1 keeps the form in the flow so going back and forth doesn't lose it. */
export const setDetails = (details) => set(details);

/**
 * Step 1 → 2: register the scanned card with the server. Re-registering the
 * same card is how a resume starts, so the server answers with what it already
 * has rather than starting over.
 */
export async function registerCard(card) {
  const { upload } = await api.createUpload({
    stationId: state.stationId,
    pulledOn: state.pulledOn,
    notes: state.notes,
    nights: card.nights.map(({ date, files, bytes, flag }) => ({
      date,
      files,
      bytes,
      ...(flag ? { flag } : {}),
    })),
  });
  sessionStorage.setItem(KEY, upload.reference);
  set({
    card,
    upload,
    status: "ready",
    filesUploaded: upload.filesUploaded,
    bytesUploaded: upload.bytesUploaded,
    error: null,
  });
  return upload;
}

/**
 * Pick up a card that is already on the server -- "Resume upload" from the
 * volunteer's home, or a reload part-way through the wizard.
 */
export async function adopt(reference) {
  if (state.upload?.reference === reference) return state.upload;
  const { upload } = await api.fetchUpload(reference);
  sessionStorage.setItem(KEY, reference);
  set({
    upload,
    stationId: upload.stationId,
    pulledOn: upload.pulledOn,
    notes: upload.notes,
    filesUploaded: upload.filesUploaded,
    bytesUploaded: upload.bytesUploaded,
    status: upload.filesUploaded > 0 ? "interrupted" : "ready",
    error: null,
  });
  return upload;
}

/** The card the pages should show, after a reload if need be. */
export async function current() {
  if (state.upload) return state.upload;
  const reference = sessionStorage.getItem(KEY);
  return reference ? adopt(reference) : null;
}

export function start() {
  if (!state.upload || state.status === "uploading") return;
  set({ status: "uploading", error: null });
  lastReport = Date.now();
  timer = setInterval(tick, TICK_MS);
}

export function pause() {
  stopClock();
  set({ status: "paused" });
  report("in_progress");
}

export function reset() {
  stopClock();
  sessionStorage.removeItem(KEY);
  state = {
    status: "idle",
    stationId: "",
    pulledOn: today(),
    notes: "",
    card: null,
    upload: null,
    filesUploaded: 0,
    bytesUploaded: 0,
    error: null,
  };
  for (const listener of listeners) listener(state);
}

function stopClock() {
  clearInterval(timer);
  timer = null;
}

function tick() {
  const { upload } = state;
  const moved = (BYTES_PER_MINUTE / 60) * (TICK_MS / 1000) * DEMO_SPEEDUP;
  const bytesUploaded = Math.min(upload.totalBytes, state.bytesUploaded + moved);
  set({ bytesUploaded, filesUploaded: filesAt(bytesUploaded) });

  if (bytesUploaded >= upload.totalBytes) {
    stopClock();
    set({ filesUploaded: upload.fileCount, status: "done" });
    report("");
    return;
  }
  if (Date.now() - lastReport >= REPORT_EVERY_MS) report("in_progress");
}

/**
 * Files are checked off as whole files, so the count follows the byte total
 * through the night-by-night manifest rather than being a straight fraction.
 */
function filesAt(bytes) {
  const nights = state.upload.nights ?? [];
  let seen = 0;
  let files = 0;
  for (const night of nights) {
    if (!night.files) continue;
    const per = night.bytes / night.files;
    const inThis = Math.floor(Math.max(0, Math.min(night.bytes, bytes - seen)) / per);
    files += inThis;
    seen += night.bytes;
    if (seen >= bytes) break;
  }
  return Math.min(state.upload.fileCount, files);
}

/** Per-night progress for the uploading screen's checklist. */
export function nightProgress() {
  const nights = state.upload?.nights ?? [];
  let before = 0;
  return nights.map((night) => {
    const done = Math.max(0, Math.min(night.files, state.filesUploaded - before));
    before += night.files;
    return { ...night, done, percent: night.files ? (done / night.files) * 100 : 0 };
  });
}

/** Minutes of transfer left at the real link speed. */
export const minutesRemaining = () =>
  ((state.upload?.totalBytes ?? 0) - state.bytesUploaded) / BYTES_PER_MINUTE;
export const minutesElapsed = () => state.bytesUploaded / BYTES_PER_MINUTE;
export const totalMinutes = (bytes) => bytes / BYTES_PER_MINUTE;
export const linkSpeed = () => `${LINK_MBPS} Mb/s`;

async function report(status) {
  lastReport = Date.now();
  const { upload, filesUploaded, bytesUploaded } = state;
  try {
    const body = await api.reportProgress(upload.reference, {
      filesUploaded: Math.round(filesUploaded),
      bytesUploaded: Math.round(bytesUploaded),
      status,
    });
    set({ upload: body.upload });
  } catch (error) {
    // A failed report is exactly what a dropped connection looks like, so it
    // is treated as one: stop, keep the count, and offer to carry on.
    stopClock();
    set({ status: "interrupted", error });
  }
}

function today() {
  return new Date().toISOString().slice(0, 10);
}
