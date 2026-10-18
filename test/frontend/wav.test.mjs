// readWavFormat (frontend/js/wav.js) against the headers recorders write.
// Run with `node --test test/frontend/`.

import assert from "node:assert/strict";
import { openAsBlob } from "node:fs";
import { test } from "node:test";

import { readWavFormat } from "../../frontend/js/wav.js";

/** A RIFF chunk, padded to an even length as the format requires. */
function chunk(id, body) {
  const out = new Uint8Array(8 + body.length + (body.length & 1));
  out.set(new TextEncoder().encode(id), 0);
  new DataView(out.buffer).setUint32(4, body.length, true);
  out.set(body, 8);
  return out;
}

/** A "fmt " chunk body; `extensible` adds the 22 bytes WAVE_FORMAT_EXTENSIBLE carries. */
function fmt({ code = 1, channels = 1, rate = 48000, bits = 16, extensible = null } = {}) {
  const body = new Uint8Array(extensible ? 40 : 16);
  const v = new DataView(body.buffer);
  const align = (channels * bits) / 8;
  v.setUint16(0, extensible ? 0xfffe : code, true);
  v.setUint16(2, channels, true);
  v.setUint32(4, rate, true);
  v.setUint32(8, rate * align, true);
  v.setUint16(12, align, true);
  v.setUint16(14, bits, true);
  if (extensible) {
    v.setUint16(16, 22, true);
    v.setUint16(18, extensible.validBits ?? bits, true);
    v.setUint16(24, extensible.code ?? 1, true);
    body.set([0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71], 26);
  }
  return chunk("fmt ", body);
}

/** A WAV of the chunks given, with RIFF's length; `dataSize` overrides what the data chunk claims. */
function wav(chunks, { dataSize } = {}) {
  const body = concat([new TextEncoder().encode("WAVE"), ...chunks]);
  const out = concat([new TextEncoder().encode("RIFF"), new Uint8Array(4), body]);
  new DataView(out.buffer).setUint32(4, body.length, true);
  if (dataSize !== undefined) {
    const at = find(out, "data");
    new DataView(out.buffer).setUint32(at + 4, dataSize, true);
  }
  return new Blob([out]);
}

function concat(parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

function find(bytes, id) {
  const want = new TextEncoder().encode(id);
  for (let i = 0; i + 4 <= bytes.length; i++) if (want.every((b, j) => bytes[i + j] === b)) return i;
  return -1;
}

const samples = (n, frameBytes) => chunk("data", new Uint8Array(n * frameBytes).map((_, i) => i));

test("plain 16-bit mono", async () => {
  const file = wav([fmt(), samples(1000, 2)]);
  assert.deepEqual(await readWavFormat(file), {
    channels: 1, sampleRate: 48000, bitsPerSample: 16, dataStart: 44, dataBytes: 2000, samples: 1000,
  });
});

test("metadata before and after the samples is walked past", async () => {
  // AudioMoth-style LIST tags, a Wildlife Acoustics "wamd" chunk of odd
  // length (so padded), GUANO, a JUNK chunk aligning the data, and tags after.
  const list = chunk("LIST", new TextEncoder().encode("INFOICMT\x20\x00\x00\x00Recorded at 23:00:00 by AudioMoth"));
  const wamd = chunk("wamd", new Uint8Array(77).fill(1));
  const guano = chunk("guan", new TextEncoder().encode("GUANO|Version: 1.0\nLoc Position: 47.66 -122.11\n"));
  const junk = chunk("JUNK", new Uint8Array(3000));
  const file = wav([list, fmt({ channels: 2 }), wamd, guano, junk, samples(500, 4), chunk("id3 ", new Uint8Array(128))]);
  const got = await readWavFormat(file);
  assert.equal(got.channels, 2);
  assert.equal(got.samples, 500);
  assert.equal(got.dataBytes, 2000);
  const bytes = new Uint8Array(await file.arrayBuffer());
  assert.equal(bytes[got.dataStart + 1], 1, "dataStart points at the samples");
  assert.equal(got.dataStart, find(bytes, "data") + 8);
});

test("24-bit, and WAVE_FORMAT_EXTENSIBLE PCM", async () => {
  assert.equal((await readWavFormat(wav([fmt({ bits: 24, channels: 2 }), samples(10, 6)]))).bitsPerSample, 24);
  const ext = await readWavFormat(wav([fmt({ bits: 24, channels: 4, extensible: { validBits: 20 } }), samples(10, 12)]));
  assert.deepEqual([ext.channels, ext.bitsPerSample, ext.samples], [4, 24, 10]);
});

test("what FLAC can't hold as it is goes as it is", async () => {
  const cases = {
    "32-bit float": wav([fmt({ code: 3, bits: 32 }), samples(10, 4)]),
    "extensible float": wav([fmt({ bits: 32, extensible: { code: 3 } }), samples(10, 4)]),
    "8-bit": wav([fmt({ bits: 8 }), samples(10, 1)]),
    "32-bit integer": wav([fmt({ bits: 32 }), samples(10, 4)]),
    "nine channels": wav([fmt({ channels: 9 }), samples(10, 18)]),
    "a sample rate past libFLAC's": wav([fmt({ rate: 768000 }), samples(10, 2)]),
    "IMA ADPCM": wav([fmt({ code: 0x11 }), samples(10, 2)]),
  };
  for (const [what, file] of Object.entries(cases)) assert.equal(await readWavFormat(file), null, what);
});

test("a recording whose header was never finished goes as it is", async () => {
  const cases = {
    // A recorder that lost power before writing the length.
    "data length 0": wav([fmt(), samples(100, 2)], { dataSize: 0 }),
    "data length past the end": wav([fmt(), samples(100, 2)], { dataSize: 0xffffffff }),
    "a partial last sample": wav([fmt({ channels: 2 }), samples(100, 4)], { dataSize: 399 }),
    "no data chunk": wav([fmt()]),
    "data before fmt": wav([samples(100, 2), fmt()]),
    "cut off inside fmt": new Blob([(await wav([fmt(), samples(1, 2)]).arrayBuffer()).slice(0, 30)]),
  };
  for (const [what, file] of Object.entries(cases)) assert.equal(await readWavFormat(file), null, what);
});

test("not a WAV", async () => {
  for (const bytes of ["fLaC\0\0\0\x22", "RIFF\x04\0\0\0AVI ", "RF64\xff\xff\xff\xffWAVE", "", "RIFF"]) {
    assert.equal(await readWavFormat(new Blob([bytes])), null, JSON.stringify(bytes));
  }
});

test("the Osprey fixture, 32-bit float, goes as it is", async () => {
  const fixture = new URL("../2026-09-09 Osprey.wav", import.meta.url);
  assert.equal(await readWavFormat(await openAsBlob(fixture)), null);
});
