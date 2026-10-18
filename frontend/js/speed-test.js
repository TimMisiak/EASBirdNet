// "Test connection" on the card check step: what a card upload is up against,
// measured one part at a time, so a slow upload can be put down to the card
// reader or to the line.
//
// - Reading the card: bytes read off the card's files as fast as the browser
//   will hand them over, sent nowhere -- as many files at once as the upload
//   sends, each streamed, because that is how the upload reads them (tus hands
//   each chunk to the browser as a Blob slice).
// - Sending to Birdsense: bytes posted to /api/v1/speedtest, which the server
//   reads and throws away -- the same route through the ingress a card takes,
//   with no storage or database behind it. Once with one request at a time,
//   and once with as many at once as the upload itself uses, because the
//   upload starts on one file and only adds more as the line proves fast
//   (upload-flow.js, filesAtOnce).
//
// Each phase runs for a few seconds or a few hundred MB, whichever comes first.

import * as api from "./api.js";
import { PARALLEL_FILES } from "./upload-flow.js";

/** How much of a file is sent at a time. */
const PIECE_BYTES = 8_000_000;
const READ = { ms: 8_000, bytes: 1_000_000_000 };
const SEND = { ms: 7_000, bytes: 500_000_000 };

/**
 * How far each file has been read, so a second test reads on from there: what
 * it already read is in the computer's file cache, and reading it again would
 * measure memory rather than the card.
 */
const readTo = new WeakMap();

/**
 * Run all three, in turn. `files` are the card's ({file, bytes}).
 * `onProgress({phase, bytesPerSecond})` is called as each phase goes, phase
 * being "read", "single" or "parallel". Resolves with bytes per second for
 * each, or null where there was nothing to measure.
 */
export async function run(files, { signal, onProgress = () => {} } = {}) {
  const read = await readCard(files, signal, (bps) => onProgress({ phase: "read", bytesPerSecond: bps }));
  const body = payload();
  const single = await send(body, 1, signal, (bps) => onProgress({ phase: "single", bytesPerSecond: bps }));
  const parallel = await send(body, PARALLEL_FILES, signal, (bps) =>
    onProgress({ phase: "parallel", bytesPerSecond: bps }),
  );
  return { read, single, parallel, streams: PARALLEL_FILES };
}

/**
 * Read the card's files the way the upload does: PARALLEL_FILES of them at
 * once, each streamed by the browser, and drop what was read. One file read a
 * piece at a time, waiting on each before asking for the next, leaves the
 * reader idle between pieces -- it measured half what the upload then reached.
 */
async function readCard(files, signal, onProgress) {
  const list = files.filter((f) => f.file && f.bytes > 0).map((f) => f.file);
  // Only what no test has read yet: past that, it's the cache again.
  const queue = list.filter((file) => (readTo.get(file) ?? 0) < file.size);
  const start = performance.now();
  let bytes = 0;
  const spent = () => performance.now() - start >= READ.ms || bytes >= READ.bytes;
  const lane = async () => {
    while (!spent() && queue.length) {
      const file = queue.shift();
      const reader = file.slice(readTo.get(file) ?? 0).stream().getReader();
      try {
        while (!spent()) {
          signal?.throwIfAborted();
          const { done, value } = await reader.read();
          if (done) break;
          bytes += value.byteLength;
          readTo.set(file, (readTo.get(file) ?? 0) + value.byteLength);
          onProgress(rate(bytes, performance.now() - start));
        }
      } finally {
        reader.cancel().catch(() => {});
      }
    }
  };
  await Promise.all(Array.from({ length: PARALLEL_FILES }, lane));
  return rate(bytes, performance.now() - start);
}

/**
 * Post pieces with `streams` requests in flight until the time or the bytes
 * are spent, then let the last ones finish: a request cut off part-way would
 * have sent bytes nobody counted.
 */
async function send(body, streams, signal, onProgress) {
  const start = performance.now();
  let bytes = 0;
  let started = 0;
  const spent = () => performance.now() - start >= SEND.ms || started * body.size >= SEND.bytes;
  const lane = async () => {
    while (!spent()) {
      started += 1;
      await api.sendSpeedTest(body, signal);
      bytes += body.size;
      onProgress(rate(bytes, performance.now() - start));
    }
  };
  await Promise.all(Array.from({ length: streams }, lane));
  return rate(bytes, performance.now() - start);
}

/** A piece of random bytes, so nothing on the way can compress it. */
function payload() {
  const bytes = new Uint8Array(PIECE_BYTES);
  // getRandomValues fills at most 64 KiB a call.
  for (let at = 0; at < bytes.length; at += 65_536) {
    crypto.getRandomValues(bytes.subarray(at, at + 65_536));
  }
  return new Blob([bytes]);
}

const rate = (bytes, ms) => (ms > 0 && bytes > 0 ? (bytes * 1_000) / ms : null);
