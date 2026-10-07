// Encodes one WAV at a time as FLAC, for flac.js, which decides what to encode
// and hands the result to the upload.
//
// A classic worker, not a module: libflac.js is a classic script that defines
// `Flac`, and only importScripts can load one. It is vendored (THIRD_PARTY_NOTICES.md)
// because nothing loaded inside a worker can carry an integrity hash.
//
// For each job it reads the WAV's samples off the card a few MB at a time,
// encodes them, and writes the FLAC to the origin private file system (OPFS)
// -- or to memory where there is none -- then patches the header with what
// libFLAC only knows at the end. When flac.js asks, which is until a file in
// the tab has passed, it then decodes the whole file again with libFLAC's MD5
// check before handing it back. That check is of the bytes that will be
// uploaded, against the MD5 the encoder took of the samples it was given: it
// covers the encoder, this file's handling of its output, and the storage in
// between, which go wrong for every file alike if they go wrong at all.

"use strict";

/** Keep in step with ENCODER in flac.js: a resumed upload trusts the bytes to be the same. */
self.FLAC_SCRIPT_LOCATION = "/vendor/libflacjs-5.6.0/";
importScripts("/vendor/libflacjs-5.6.0/libflac.min.wasm.js");

/** libFLAC's default preset: within a fraction of a percent of level 8, at a third of the time. */
const LEVEL = 5;
/** How much of the card is read and encoded at a time. */
const READ_BYTES = 4 << 20;
/** "fLaC", STREAMINFO's block header and STREAMINFO: the bytes patched at the end. */
const HEADER_BYTES = 42;
/** How much a memory sink grows by. */
const MEMORY_BLOCK = 1 << 20;
/** Where the FLACs are written in OPFS, one directory per tab (see flac.js). */
const SPOOL = "birdsense-flac";

/** Samples are read with typed arrays, which are in the machine's byte order; a WAV's are little-endian. */
const littleEndian = new Uint8Array(new Uint16Array([1]).buffer)[0] === 1;
const ready = new Promise((resolve) => (Flac.isReady() ? resolve() : Flac.on("ready", resolve)));
const cancelled = new Set();

// A .wasm that won't load -- blocked, missing -- rejects inside libflac.js
// where nothing catches it, and `ready` never comes. Say so, or flac.js would
// wait on this worker for good.
self.addEventListener("unhandledrejection", (event) => {
  self.postMessage({ fatal: String(event.reason?.message ?? event.reason) });
});

class Cancelled extends Error {}

self.onmessage = async ({ data }) => {
  if ("cancel" in data) {
    cancelled.add(data.cancel);
    return;
  }
  const { id } = data;
  try {
    await ready;
    const result = await encode(
      data,
      () => cancelled.has(id),
      (read) => self.postMessage({ id, read }),
    );
    self.postMessage({ id, ok: true, ...result });
  } catch (error) {
    self.postMessage({ id, ok: false, error: error instanceof Cancelled ? "cancelled" : String(error?.message ?? error) });
  } finally {
    cancelled.delete(id);
  }
};

/**
 * One file: {file, format (wav.js), spool: {tab, name} or null, check}. Answers
 * the FLAC as a File (OPFS) or Blob (memory), its length, and a hash of its
 * bytes for the upload's fingerprint. `check` decodes it again before it is
 * handed back. `onRead` hears how many of the WAV's sample bytes have been
 * read and encoded so far, after each read: what flac.js measures the
 * encoder's speed by.
 */
async function encode({ file, format, spool, check: checking }, isCancelled, onRead = () => {}) {
  if (!littleEndian) throw new Error("this machine's byte order isn't a WAV's");
  const { channels, sampleRate, bitsPerSample, dataStart, samples } = format;
  const sink = await openSink(spool);
  try {
    const head = new Uint8Array(HEADER_BYTES);
    // FNV-1a over the bytes as they are written, the header last: it is only
    // final once the encode is.
    let hash = 0x811c9dc5;
    let metadata = null;
    let sinkError = null;
    const encoder = Flac.create_libflac_encoder(sampleRate, channels, bitsPerSample, LEVEL, samples, false, 0);
    if (!encoder) throw new Error("the encoder couldn't be created");
    try {
      const status = Flac.init_encoder_stream(
        encoder,
        (data) => {
          // Thrown here, libFLAC stops with a fatal error.
          try {
            const toHead = HEADER_BYTES - sink.size;
            if (toHead > 0) head.set(data.subarray(0, toHead), sink.size);
            hash = fnv(hash, toHead > 0 ? data.subarray(toHead) : data);
            sink.append(data);
          } catch (error) {
            sinkError = error;
            throw error;
          }
        },
        (m) => (metadata = m),
      );
      if (status !== 0) throw new Error(`the encoder wouldn't start (status ${status})`);

      const frameBytes = (channels * bitsPerSample) / 8;
      const perRead = Math.floor(READ_BYTES / frameBytes);
      const pcm = new Int32Array(perRead * channels);
      for (let done = 0; done < samples; done += perRead) {
        if (isCancelled()) throw new Cancelled();
        const n = Math.min(perRead, samples - done);
        const at = dataStart + done * frameBytes;
        const bytes = new Uint8Array(await file.slice(at, at + n * frameBytes).arrayBuffer());
        if (bytes.length !== n * frameBytes) throw new Error("the file on the card came up short");
        const values = pcm.subarray(0, n * channels);
        widen(bytes, bitsPerSample, values);
        if (!Flac.FLAC__stream_encoder_process_interleaved(encoder, values, n)) {
          throw sinkError ?? new Error(`encoding failed (state ${Flac.FLAC__stream_encoder_get_state(encoder)})`);
        }
        onRead((done + n) * frameBytes);
      }
      if (!Flac.FLAC__stream_encoder_finish(encoder)) {
        throw sinkError ?? new Error(`finishing the encode failed (state ${Flac.FLAC__stream_encoder_get_state(encoder)})`);
      }
    } finally {
      Flac.FLAC__stream_encoder_delete(encoder);
    }

    if (metadata?.total_samples !== samples || !/^[0-9a-f]{32}$/.test(metadata.md5sum) || /^0+$/.test(metadata.md5sum)) {
      throw new Error("the encoder didn't account for every sample");
    }
    const patched = patchStreamInfo(head, metadata);
    sink.writeAt(patched, 0);
    hash = fnv(hash, patched);
    if (isCancelled()) throw new Cancelled();
    if (checking) check(sink);
    const size = sink.size;
    return { blob: await sink.finish(), bytes: size, hash: (hash >>> 0).toString(16).padStart(8, "0") };
  } catch (error) {
    await sink.discard();
    throw error;
  }
}

/** Card samples, little-endian 16- or 24-bit, as the Int32s libFLAC takes. */
function widen(bytes, bitsPerSample, out) {
  if (bitsPerSample === 16) {
    out.set(new Int16Array(bytes.buffer, bytes.byteOffset, out.length));
    return;
  }
  for (let i = 0, j = 0; i < out.length; i++, j += 3) {
    out[i] = ((bytes[j] | (bytes[j + 1] << 8) | (bytes[j + 2] << 16)) << 8) >> 8;
  }
}

/**
 * The file's first 42 bytes as they should be. libFLAC writes STREAMINFO
 * before it has seen a sample and can't seek back over a stream, so the frame
 * sizes and the MD5 -- and the total, though it was given it up front -- come
 * from the metadata it reports when it finishes. A header left with no total
 * is one libsndfile can't read (see checkFLAC on the server).
 */
function patchStreamInfo(head, m) {
  const out = head.slice();
  if (String.fromCharCode(...out.subarray(0, 4)) !== "fLaC" || (out[4] & 0x7f) !== 0) {
    throw new Error("the encoder didn't start with STREAMINFO");
  }
  const view = new DataView(out.buffer);
  const info = 8;
  view.setUint8(info + 4, m.min_framesize >>> 16);
  view.setUint16(info + 5, m.min_framesize & 0xffff);
  view.setUint8(info + 7, m.max_framesize >>> 16);
  view.setUint16(info + 8, m.max_framesize & 0xffff);
  // Total samples is 36 bits: the low nibble of byte 13, then bytes 14-17.
  view.setUint8(info + 13, (out[info + 13] & 0xf0) | (Math.floor(m.total_samples / 2 ** 32) & 0x0f));
  view.setUint32(info + 14, m.total_samples >>> 0);
  for (let i = 0; i < 16; i++) out[info + 18 + i] = parseInt(m.md5sum.slice(i * 2, i * 2 + 2), 16);
  return out;
}

/** FNV-1a, carried on from `hash` over `bytes`: a resumed upload only carries on from bytes this same. */
function fnv(hash, bytes) {
  for (let i = 0; i < bytes.length; i++) hash = Math.imul(hash ^ bytes[i], 0x01000193);
  return hash;
}

/**
 * Decode the whole FLAC as it will be uploaded, with libFLAC's MD5 check on,
 * and throw unless it comes back as the samples that went in.
 */
function check(sink) {
  const chunks = sink.chunks();
  let chunk = new Uint8Array(0);
  let pos = 0;
  let errors = 0;
  const decoder = Flac.create_libflac_decoder(true);
  try {
    Flac.FLAC__stream_decoder_set_md5_checking(decoder, true);
    const status = Flac.init_decoder_stream(
      decoder,
      (wanted) => {
        while (pos === chunk.length) {
          const next = chunks.next();
          if (next.done) return { buffer: undefined, readDataLength: 0, error: false };
          chunk = next.value;
          pos = 0;
        }
        const buffer = chunk.subarray(pos, pos + wanted);
        pos += buffer.length;
        return { buffer, readDataLength: buffer.length, error: false };
      },
      () => {},
      () => errors++,
    );
    if (status !== 0) throw new Error(`the decoder wouldn't start (status ${status})`);
    const decoded = Flac.FLAC__stream_decoder_process_until_end_of_stream(decoder);
    // finish is where libFLAC compares the MD5 of what it decoded.
    if (!Flac.FLAC__stream_decoder_finish(decoder) || !decoded || errors) {
      throw new Error("the FLAC didn't decode back to the card's samples");
    }
  } finally {
    Flac.FLAC__stream_decoder_delete(decoder);
  }
}

/** Where the FLAC is written: OPFS when flac.js has a spool for this tab, memory otherwise. */
async function openSink(spool) {
  if (spool) {
    try {
      return await opfsSink(spool);
    } catch {
      // No sync access in this browser, or no room: memory does.
    }
  }
  return memorySink();
}

async function opfsSink({ tab, name }) {
  const root = await navigator.storage.getDirectory();
  const dir = await (await root.getDirectoryHandle(SPOOL)).getDirectoryHandle(tab);
  const handle = await dir.getFileHandle(name, { create: true });
  const access = await handle.createSyncAccessHandle();
  // Before Chrome 108 these were async; awaiting either is fine.
  await access.truncate(0);
  let size = 0;
  let open = true;
  return {
    get size() {
      return size;
    },
    append(bytes) {
      if (access.write(bytes, { at: size }) !== bytes.length) throw new Error("the disk is full");
      size += bytes.length;
    },
    writeAt(bytes, at) {
      access.write(bytes, { at });
    },
    *chunks() {
      for (let at = 0; at < size; ) {
        const buffer = new Uint8Array(Math.min(MEMORY_BLOCK, size - at));
        at += access.read(buffer, { at });
        yield buffer;
      }
    },
    async finish() {
      await access.flush();
      await access.close();
      open = false;
      return handle.getFile();
    },
    async discard() {
      if (open) await access.close();
      open = false;
      await dir.removeEntry(name).catch(() => {});
    },
  };
}

function memorySink() {
  const blocks = [];
  let size = 0;
  return {
    get size() {
      return size;
    },
    append(bytes) {
      for (let from = 0; from < bytes.length; ) {
        let last = blocks[blocks.length - 1];
        if (!last || last.used === last.bytes.length) blocks.push((last = { bytes: new Uint8Array(MEMORY_BLOCK), used: 0 }));
        const n = Math.min(bytes.length - from, last.bytes.length - last.used);
        last.bytes.set(bytes.subarray(from, from + n), last.used);
        last.used += n;
        from += n;
      }
      size += bytes.length;
    },
    // Only ever the header, which is inside the first block.
    writeAt(bytes, at) {
      blocks[0].bytes.set(bytes, at);
    },
    *chunks() {
      for (const b of blocks) yield b.bytes.subarray(0, b.used);
    },
    async finish() {
      return new Blob([...this.chunks()], { type: "audio/flac" });
    },
    async discard() {
      blocks.length = 0;
    },
  };
}
