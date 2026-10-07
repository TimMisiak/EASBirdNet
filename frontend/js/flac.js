// Sending a card's WAVs as FLAC: the same samples in about half the bytes, so
// about half the time on a volunteer's uplink. The upload (upload-flow.js)
// asks for each file just before it goes; this reads the WAV's header
// (wav.js) and has a worker (flac-worker.js) encode and check it. Whatever
// can't be encoded -- not a WAV FLAC can hold, a browser that can't run the
// encoder, an encode that fails its check -- answers null and is sent as it
// is on the card, so nothing here can stop a card going.
//
// The server holds a FLAC to the WAV's sample count when it lands (checkFLAC,
// backend/internal/api/tus.go), and refuses one that doesn't match; the upload
// then sends that file as it is.

import { readWavFormat } from "./wav.js";

/**
 * What makes the bytes. A resumed upload carries on from bytes the server
 * already has, so it is only resumed by the same encoder at the same setting
 * (its fingerprint names this, and a hash of the FLAC). Keep in step with
 * flac-worker.js.
 */
export const ENCODER = "libflacjs-5.6.0/5";

/** Workers encoding at once. Each does ~25 MB of WAV a second, encode and check together. */
const WORKERS = Math.max(1, Math.min(4, (navigator.hardwareConcurrency || 2) - 1));
/** Where encoded files wait for their upload, in the origin private file system. */
const SPOOL = "birdsense-flac";
/** The speed is averaged over this much encoding time, as the line's is over that much time (upload-flow.js). */
const SPEED_WINDOW_MS = 15_000;

/**
 * How fast the card is being compressed: bytes of WAV read and encoded,
 * against the time at least one worker was at it. Time the workers spend
 * waiting for the line doesn't count against them, and workers running at
 * once add up, so on a line the encoders can't keep up with this is the speed
 * they cap the upload at, in the card's bytes. A file's check, after its
 * encode, is time with no bytes, so it counts in the speed too.
 */
const meter = { bytes: 0, busyMs: 0, since: 0, samples: [] };

const workers = [];
const queue = [];
let nextId = 0;
let nextName = 0;
/** Set once the encoder has failed to load: every file then goes as it is. */
let broken = false;
let spool;

/**
 * Encode a card's file as FLAC, ready to send. Answers {blob, samples, bytes,
 * hash, discard}, or null when the file is to go as it is. `discard` removes
 * the encoded copy, once it is sent or no longer wanted. Aborting the signal
 * stops the encode, and the promise rejects.
 * @param {File} file
 * @param {AbortSignal} signal
 */
export async function encode(file, signal) {
  if (broken) return null;
  let format;
  try {
    format = await readWavFormat(file);
  } catch {
    format = null; // Sending it as it is reports a card that can't be read.
  }
  signal.throwIfAborted();
  if (!format) return null;

  const dir = await spoolDir();
  const name = `${nextName++}.flac`;
  const job = { file, format, spool: dir && { tab: dir.tab, name } };
  try {
    const { blob, bytes, hash } = await run(job, signal);
    return {
      blob, bytes, hash, samples: format.samples,
      discard: () => dir?.handle.removeEntry(name).catch(() => {}),
    };
  } catch (error) {
    if (signal.aborted) throw error;
    console.warn(`Sending ${file.name} as it is on the card: ${error.message}`);
    return null;
  }
}

/** Run a job on the next free worker, starting one if there's room for it. */
function run(job, signal) {
  return new Promise((resolve, reject) => {
    const pending = { job, resolve, reject, worker: null, id: nextId++ };
    const abort = () => {
      pending.aborted = true;
      const i = queue.indexOf(pending);
      if (i >= 0) queue.splice(i, 1);
      pending.worker?.instance.postMessage({ cancel: pending.id });
      reject(signal.reason);
    };
    if (signal.aborted) return abort();
    signal.addEventListener("abort", abort, { once: true });
    pending.done = () => signal.removeEventListener("abort", abort);
    queue.push(pending);
    pump();
  });
}

function pump() {
  while (queue.length) {
    const worker = workers.find((w) => !w.busy) ?? (workers.length < WORKERS ? startWorker() : null);
    if (!worker) return;
    const pending = queue.shift();
    worker.busy = pending;
    pending.worker = worker;
    clock();
    worker.instance.postMessage({ id: pending.id, ...pending.job });
  }
}

function startWorker() {
  const worker = { instance: new Worker("/js/flac-worker.js"), busy: null };
  worker.instance.onmessage = ({ data }) => {
    if (data.fatal) return giveUp(data.fatal);
    const pending = worker.busy;
    if (!pending || pending.id !== data.id) return;
    if (data.read !== undefined) return measure(pending, data.read);
    worker.busy = null;
    clock();
    pending.done();
    if (data.ok && pending.aborted) unspool(pending.job);
    if (data.ok) pending.resolve(data);
    else pending.reject(new Error(data.error));
    pump();
  };
  worker.instance.onerror = (event) => {
    event.preventDefault();
    giveUp(event.message || "the worker failed");
  };
  workers.push(worker);
  return worker;
}

/**
 * A worker that can't load the encoder -- a policy that blocks it, a browser
 * without WebAssembly -- would fail every file the same way, so every file
 * from here on goes as it is.
 */
function giveUp(why) {
  if (broken) return;
  broken = true;
  console.warn(`The FLAC encoder didn't load (${why}); sending files as they are on the card.`);
  const failed = new Error("the encoder didn't load");
  for (const w of workers.splice(0)) {
    w.instance.terminate();
    w.busy?.done();
    w.busy?.reject(failed);
  }
  clock();
  for (const pending of queue.splice(0)) {
    pending.done();
    pending.reject(failed);
  }
}

/** Run the meter's clock while any worker has a job, and stop it while none does. */
function clock() {
  const now = performance.now();
  if (meter.since) meter.busyMs += now - meter.since;
  meter.since = workers.some((w) => w.busy) ? now : 0;
}

const busyMs = () => meter.busyMs + (meter.since ? performance.now() - meter.since : 0);

/** A job has read and encoded `read` bytes of its WAV so far. */
function measure(pending, read) {
  meter.bytes += read - (pending.read ?? 0);
  pending.read = read;
  const t = busyMs();
  meter.samples.push([t, meter.bytes]);
  while (meter.samples.length > 2 && t - meter.samples[1][0] >= SPEED_WINDOW_MS) meter.samples.shift();
}

/** Bytes of WAV compressed per second, over the last few seconds of encoding, or null until there's enough to say. */
export function bytesPerSecond() {
  if (meter.samples.length < 2) return null;
  const [t0, b0] = meter.samples[0];
  const [t1, b1] = meter.samples[meter.samples.length - 1];
  return t1 - t0 >= 2_000 ? ((b1 - b0) * 1_000) / (t1 - t0) : null;
}

/** Bytes of WAV compressed per second over all the encoding since resetMeter, or null if there was too little. */
export function averageBytesPerSecond() {
  const ms = busyMs();
  return ms >= 2_000 && meter.bytes > 0 ? (meter.bytes * 1_000) / ms : null;
}

/** Start measuring afresh, for another card. */
export function resetMeter() {
  meter.bytes = 0;
  meter.busyMs = 0;
  meter.since = meter.since && performance.now();
  meter.samples = [];
}

/** Remove the encoded copy of a job nobody wants any more. */
function unspool({ spool: where }) {
  if (where) spool.then((dir) => dir?.handle.removeEntry(where.name).catch(() => {}));
}

/**
 * This tab's directory for encoded files, or null to keep them in memory.
 * OPFS keeps a few hundred MB of FLAC off the heap while it waits its turn.
 * Each tab holds a Web Lock named for its directory for as long as it is
 * open, so a tab can clear out what closed tabs left without touching a
 * directory another open tab is uploading from.
 */
function spoolDir() {
  spool ??= (async () => {
    try {
      if (!navigator.locks || !navigator.storage?.getDirectory) return null;
      const tab = crypto.randomUUID();
      await new Promise((held) => {
        navigator.locks.request(`${SPOOL}:${tab}`, () => {
          held();
          return new Promise(() => {}); // held until the tab goes
        });
      });
      const root = await navigator.storage.getDirectory();
      const all = await root.getDirectoryHandle(SPOOL, { create: true });
      const live = new Set((await navigator.locks.query()).held.map((lock) => lock.name));
      for await (const name of all.keys()) {
        if (!live.has(`${SPOOL}:${name}`)) await all.removeEntry(name, { recursive: true }).catch(() => {});
      }
      return { tab, handle: await all.getDirectoryHandle(tab, { create: true }) };
    } catch {
      // A private window, or a browser without OPFS.
      return null;
    }
  })();
  return spool;
}
