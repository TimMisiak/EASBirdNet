// Reading a WAV's header, to know whether it can be sent as FLAC (flac.js).
//
// A WAV is RIFF chunks: "fmt " says what the samples are, "data" holds them,
// and a recorder writes whatever else it likes around those two -- a LIST of
// tags, GUANO or Wildlife Acoustics metadata, padding to line the samples up
// on a sector. Only "fmt " and "data" are read; the rest is walked past, and
// isn't carried into the FLAC.

/** The most chunks walked looking for "data": a header longer than this isn't one we know. */
const MAX_CHUNKS = 64;
const FORMAT_PCM = 0x0001;
const FORMAT_EXTENSIBLE = 0xfffe;
/** KSDATAFORMAT_SUBTYPE_PCM after its first two bytes, which are the format code. */
const PCM_GUID_TAIL = [0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71];
/** The highest sample rate libFLAC 1.3 will encode. */
const MAX_SAMPLE_RATE = 655_350;

/**
 * What a WAV holds, when FLAC can hold exactly the same: integer PCM at 16 or
 * 24 bits, one to eight channels. `samples` is per channel, and is what the
 * server holds the finished FLAC to. Anything else answers null and is sent as
 * it is: float, 8- or 32-bit, compressed, RF64, not a WAV at all, or a
 * recording whose header was never finished -- a recorder that lost power
 * part way leaves the data length at 0, or longer than the file.
 * @param {Blob} file
 * @returns {Promise<{channels: number, sampleRate: number, bitsPerSample: number,
 *   dataStart: number, dataBytes: number, samples: number} | null>}
 */
export async function readWavFormat(file) {
  const read = async (at, length) => new DataView(await file.slice(at, at + length).arrayBuffer());
  if (file.size < 12) return null;
  const riff = await read(0, 12);
  if (tag(riff, 0) !== "RIFF" || tag(riff, 8) !== "WAVE") return null;

  let format = null;
  let at = 12;
  for (let i = 0; i < MAX_CHUNKS && at + 8 <= file.size; i++) {
    const head = await read(at, 8);
    const id = tag(head, 0);
    const size = head.getUint32(4, true);
    if (id === "fmt ") {
      format = size >= 16 ? formatOf(await read(at + 8, Math.min(size, 40)), size) : null;
      if (!format) return null;
    } else if (id === "data") {
      const dataStart = at + 8;
      if (!format || size === 0 || dataStart + size > file.size || size % format.blockAlign) return null;
      const { channels, sampleRate, bitsPerSample, blockAlign } = format;
      return { channels, sampleRate, bitsPerSample, dataStart, dataBytes: size, samples: size / blockAlign };
    }
    // Chunks are padded to an even length; the size doesn't count the pad.
    at += 8 + size + (size & 1);
  }
  return null;
}

/** The "fmt " chunk, if it is integer PCM FLAC can hold; else null. */
function formatOf(fmt, size) {
  if (fmt.byteLength < 16) return null; // the file ends inside the chunk
  let code = fmt.getUint16(0, true);
  const channels = fmt.getUint16(2, true);
  const sampleRate = fmt.getUint32(4, true);
  const blockAlign = fmt.getUint16(12, true);
  const bitsPerSample = fmt.getUint16(14, true);
  if (code === FORMAT_EXTENSIBLE) {
    // The real format is the subformat GUID's first two bytes. A container
    // wider than its valid bits holds them left-justified, which encodes as
    // the container's width without losing anything.
    if (size < 40 || fmt.byteLength < 40) return null;
    code = fmt.getUint16(24, true);
    if (PCM_GUID_TAIL.some((b, i) => fmt.getUint8(26 + i) !== b)) return null;
  }
  const ok =
    code === FORMAT_PCM &&
    (bitsPerSample === 16 || bitsPerSample === 24) &&
    channels >= 1 && channels <= 8 &&
    sampleRate >= 1 && sampleRate <= MAX_SAMPLE_RATE &&
    blockAlign === (channels * bitsPerSample) / 8;
  return ok ? { channels, sampleRate, bitsPerSample, blockAlign } : null;
}

function tag(view, at) {
  return String.fromCharCode(view.getUint8(at), view.getUint8(at + 1), view.getUint8(at + 2), view.getUint8(at + 3));
}
