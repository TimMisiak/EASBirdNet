// frontend/js/flac-worker.js, with the vendored libflac.js, run the way a
// browser runs it: a classic script in a global of its own that has
// importScripts, postMessage and fetch. Without OPFS here it writes to memory.
// Run with `node --test test/frontend/`.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const frontend = new URL("../../frontend/", import.meta.url);

/** The worker in a global of its own, and a function that sends it a job and waits for the answer. */
function startWorker() {
  const answers = new Map();
  // Not this realm's WebAssembly: libflac.js recognizes the error a wasm table
  // throws with `instanceof TypeError`, and that only holds when the table and
  // TypeError come from the same realm -- the context's own.
  const global = {
    console, Blob, Response, URL, TextDecoder, setTimeout, clearTimeout,
    location: { href: new URL("js/flac-worker.js", "http://localhost/").href },
    importScripts: (...paths) => {
      for (const p of paths) vm.runInContext(readFileSync(new URL(`.${p}`, frontend), "utf8"), context, { filename: p });
    },
    fetch: async (url) =>
      new Response(readFileSync(new URL(`.${new URL(url, "http://localhost/").pathname}`, frontend)), {
        headers: { "Content-Type": "application/wasm" },
      }),
    postMessage: (data) => answers.get(data.id)?.(data),
    addEventListener: () => {},
  };
  global.self = global;
  const context = vm.createContext(global);
  vm.runInContext(readFileSync(new URL("js/flac-worker.js", frontend), "utf8"), context, { filename: "flac-worker.js" });
  const send = (job) =>
    new Promise((resolve) => {
      const id = Math.random();
      answers.set(id, resolve);
      context.onmessage({ data: { id, ...job } });
    });
  return { send, context };
}

/** A WAV of `frames` sample frames of noise-like samples, and its format as wav.js reports it. */
function wav({ channels, bits, frames, rate = 48000 }) {
  const frameBytes = (channels * bits) / 8;
  const header = 44;
  const bytes = new Uint8Array(header + frames * frameBytes);
  const v = new DataView(bytes.buffer);
  bytes.set(new TextEncoder().encode("RIFF"), 0);
  v.setUint32(4, bytes.length - 8, true);
  bytes.set(new TextEncoder().encode("WAVEfmt "), 8);
  v.setUint32(16, 16, true);
  v.setUint16(20, 1, true);
  v.setUint16(22, channels, true);
  v.setUint32(24, rate, true);
  v.setUint32(28, rate * frameBytes, true);
  v.setUint16(32, frameBytes, true);
  v.setUint16(34, bits, true);
  bytes.set(new TextEncoder().encode("data"), 36);
  v.setUint32(40, frames * frameBytes, true);
  // A tone with noise on it, full scale: compressible, but not trivially.
  let seed = 7;
  const max = 2 ** (bits - 1);
  for (let i = 0; i < frames * channels; i++) {
    seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
    const value = Math.round(Math.sin(i / 23) * max * 0.6 + ((seed / 2 ** 32) - 0.5) * max * 0.3);
    const s = Math.max(-max, Math.min(max - 1, value));
    for (let b = 0; b < bits / 8; b++) bytes[header + i * (bits / 8) + b] = (s >> (8 * b)) & 0xff;
  }
  const format = { channels, sampleRate: rate, bitsPerSample: bits, dataStart: header, dataBytes: frames * frameBytes, samples: frames };
  return { file: new Blob([bytes]), format, data: bytes.subarray(header) };
}

/** STREAMINFO's sample rate, channels, bits per sample, total samples and MD5. */
function streamInfo(flac) {
  assert.equal(new TextDecoder().decode(flac.subarray(0, 4)), "fLaC");
  const v = new DataView(flac.buffer, flac.byteOffset + 8, 34);
  const hi = v.getUint32(10);
  return {
    sampleRate: hi >>> 12,
    channels: ((hi >>> 9) & 7) + 1,
    bitsPerSample: ((hi >>> 4) & 0x1f) + 1,
    total: (hi & 0xf) * 2 ** 32 + v.getUint32(14),
    md5: Buffer.from(flac.subarray(8 + 18, 8 + 34)).toString("hex"),
  };
}

for (const shape of [
  { channels: 1, bits: 16, frames: 3_000_000 }, // more than one 4 MB read
  { channels: 2, bits: 16, frames: 100_001 },
  { channels: 2, bits: 24, frames: 777_777 },
]) {
  test(`encodes ${shape.channels}-channel ${shape.bits}-bit as FLAC that holds the same samples`, async () => {
    const { send } = startWorker();
    const { file, format, data } = wav(shape);
    const got = await send({ file, format, spool: null });
    assert.ok(got.ok, got.error);
    const flac = new Uint8Array(await got.blob.arrayBuffer());
    assert.equal(got.bytes, flac.length);
    assert.ok(flac.length < data.length, "smaller than the samples");
    // FLAC's MD5 is of the samples as little-endian integers at their own
    // width: for 16- and 24-bit PCM, exactly the WAV's data chunk. The worker
    // has already decoded the file against it.
    assert.deepEqual(streamInfo(flac), {
      sampleRate: 48000, channels: shape.channels, bitsPerSample: shape.bits,
      total: shape.frames, md5: createHash("md5").update(data).digest("hex"),
    });
    assert.match(got.hash, /^[0-9a-f]{8}$/);
  });
}

test("the same samples encode to the same bytes, which a resumed upload relies on", async () => {
  const { file, format } = wav({ channels: 1, bits: 16, frames: 200_000 });
  const a = await startWorker().send({ file, format, spool: null });
  const b = await startWorker().send({ file, format, spool: null });
  assert.equal(a.hash, b.hash);
  assert.deepEqual(new Uint8Array(await a.blob.arrayBuffer()), new Uint8Array(await b.blob.arrayBuffer()));
});

test("a file shorter than its header says fails rather than encoding what there is", async () => {
  const { send } = startWorker();
  const { file, format } = wav({ channels: 1, bits: 16, frames: 50_000 });
  const got = await send({ file: file.slice(0, file.size - 1000), format, spool: null });
  assert.equal(got.ok, false);
});

test("the check refuses a FLAC with any byte changed", async () => {
  const { send, context } = startWorker();
  const { file, format } = wav({ channels: 1, bits: 16, frames: 200_000 });
  const got = await send({ file, format, spool: null });
  const flac = new Uint8Array(await got.blob.arrayBuffer());
  const sinkOf = (bytes) => {
    const sink = context.memorySink();
    sink.append(bytes);
    return sink;
  };
  assert.equal(context.check(sinkOf(flac)), got.hash);
  for (const at of [100, Math.floor(flac.length / 2), flac.length - 3]) {
    const bad = flac.slice();
    bad[at] ^= 0x10;
    assert.throws(() => context.check(sinkOf(bad)), /didn't decode back/, `byte ${at} flipped`);
  }
  assert.throws(() => context.check(sinkOf(flac.subarray(0, flac.length - 500))), /didn't decode back/, "cut short");
});
