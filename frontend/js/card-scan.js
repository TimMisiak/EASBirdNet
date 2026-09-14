// Reading an SD card in the browser.
//
// The card is never copied to the volunteer's disk: we ask for the folder, walk
// it, and keep file handles. What comes back is a manifest -- how many nights,
// how many files, how big -- which is what the "here's what's on the card"
// screen is built on.

const AUDIO = /\.(wav|flac|mp3|w4v)$/i;

/** Thrown when the volunteer closes the picker without choosing anything. */
export class CancelledError extends Error {
  constructor() {
    super("no card chosen");
    this.name = "CancelledError";
  }
}

/**
 * Ask for the card and read its manifest.
 * @returns {Promise<{label: string, nights: object[], files: object[], fileCount: number, totalBytes: number, skipped: string[]}>}
 */
export async function scanCard() {
  const files = window.showDirectoryPicker ? await viaDirectoryPicker() : await viaInput();
  return manifest(files);
}

// The modern path: a real directory handle, so the files stay on the card and
// a resume can re-read them without asking again.
async function viaDirectoryPicker() {
  let handle;
  try {
    handle = await window.showDirectoryPicker({ id: "birdsense-card", mode: "read" });
  } catch {
    throw new CancelledError();
  }
  const files = [];
  await walk(handle, files);
  return { label: handle.name, entries: files };
}

async function walk(dir, out, depth = 0) {
  // Recorders write one flat folder, sometimes one folder per deployment.
  // Anything deeper than that is not a card we understand.
  if (depth > 3) return;
  for await (const entry of dir.values()) {
    if (entry.kind === "directory") {
      await walk(entry, out, depth + 1);
    } else {
      out.push(await entry.getFile());
    }
  }
}

// The fallback path: <input webkitdirectory>, which every current browser
// supports. It reads the whole folder into File objects up front.
function viaInput() {
  return new Promise((resolve, reject) => {
    const input = document.createElement("input");
    input.type = "file";
    input.webkitdirectory = true;
    input.multiple = true;
    input.style.display = "none";
    document.body.append(input);

    input.addEventListener(
      "change",
      () => {
        const entries = [...input.files];
        input.remove();
        if (!entries.length) return reject(new CancelledError());
        const first = entries[0].webkitRelativePath?.split("/")[0];
        resolve({ label: first || "SD card", entries });
      },
      { once: true },
    );
    // There is no reliable "cancelled" event; the picker just never fires
    // change. The input is removed on the next scan either way.
    input.click();
  });
}

function manifest({ label, entries }) {
  const audio = [];
  const skipped = [];
  for (const file of entries) {
    if (AUDIO.test(file.name)) audio.push(file);
    else skipped.push(file.name);
  }

  const byNight = new Map();
  for (const file of audio) {
    const date = nightOf(file.lastModified);
    const night = byNight.get(date) ?? { date, files: 0, bytes: 0 };
    night.files += 1;
    night.bytes += file.size;
    byNight.set(date, night);
  }

  const nights = [...byNight.values()].sort((a, b) => a.date.localeCompare(b.date));
  flagOddNights(nights);

  return {
    label,
    nights,
    files: audio,
    fileCount: audio.length,
    totalBytes: audio.reduce((sum, f) => sum + f.size, 0),
    skipped,
  };
}

/**
 * Which night a file belongs to. Recording runs dusk to dawn, so a 3 a.m. file
 * is part of the previous evening's night -- shifting back 12 hours puts the
 * whole night on one date.
 */
function nightOf(timestamp) {
  const t = new Date(timestamp - 12 * 3600 * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())}`;
}

/**
 * Mark the nights worth a second look. A night with far fewer files than its
 * neighbours usually means flat batteries or a knocked-over recorder, and the
 * last night is normally short because that's the morning the card was pulled.
 */
function flagOddNights(nights) {
  if (nights.length < 3) return;
  const counts = [...nights].map((n) => n.files).sort((a, b) => a - b);
  const typical = counts[Math.floor(counts.length / 2)];
  nights.forEach((night, i) => {
    if (night.files >= typical * 0.75) return;
    night.flag = i === nights.length - 1 ? "partial" : "short";
  });
}

/**
 * A stand-in card, for trying the flow without an SD card in the reader.
 * Fourteen nights, two of them odd, matching a full SwiftOne deployment.
 */
export function sampleCard() {
  const nights = [];
  const start = new Date();
  start.setDate(start.getDate() - 21);
  for (let i = 0; i < 14; i += 1) {
    const day = new Date(start);
    day.setDate(start.getDate() + i);
    const files = i === 6 ? 11 : i === 13 ? 3 : 24;
    nights.push({
      date: nightOf(day.getTime() + 12 * 3600 * 1000),
      files,
      bytes: files * 383_000_000,
      flag: i === 6 ? "short" : i === 13 ? "partial" : undefined,
    });
  }
  return {
    label: "SWIFT01",
    nights,
    files: [],
    fileCount: nights.reduce((n, x) => n + x.files, 0),
    totalBytes: nights.reduce((n, x) => n + x.bytes, 0),
    skipped: ["CONFIG.TXT", ".DS_Store"],
  };
}
