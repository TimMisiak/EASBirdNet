// Reading an SD card in the browser.
//
// The card is never copied to the volunteer's disk: we ask for the folder, walk
// it, and keep file handles. What comes back is a manifest -- how many nights,
// how many files, how big -- which is what the "here's what's on the card"
// screen is built on, and the list of files the upload sends.

const AUDIO = /\.(wav|flac|mp3|w4v)$/i;

/** Thrown when the volunteer closes the picker without choosing anything. */
export class CancelledError extends Error {
  constructor() {
    super("no card chosen");
    this.name = "CancelledError";
  }
}

/**
 * Ask for the card and read its manifest. Each of `files` is
 * {path, bytes, night, file}: the path from the card's root with forward
 * slashes, and the File to send.
 * @returns {Promise<{label: string, nights: object[], files: object[], fileCount: number, totalBytes: number, skipped: string[]}>}
 */
export async function scanCard() {
  const found = window.showDirectoryPicker ? await viaDirectoryPicker() : await viaInput();
  return manifest(found);
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
  const entries = [];
  await walk(handle, entries);
  return { label: handle.name, entries };
}

async function walk(dir, out, prefix = "", depth = 0) {
  // Recorders write one flat folder, sometimes one folder per deployment.
  // Anything deeper than that is not a card we understand.
  if (depth > 3) return;
  for await (const entry of dir.values()) {
    if (entry.kind === "directory") {
      await walk(entry, out, `${prefix}${entry.name}/`, depth + 1);
    } else {
      out.push({ path: prefix + entry.name, file: await entry.getFile() });
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
        const files = [...input.files];
        input.remove();
        if (!files.length) return reject(new CancelledError());
        // Relative paths start with the folder that was picked; the card's
        // paths start inside it.
        const [label] = files[0].webkitRelativePath.split("/");
        const entries = files.map((file) => ({
          path: file.webkitRelativePath.split("/").slice(1).join("/") || file.name,
          file,
        }));
        resolve({ label: label || "SD card", entries });
      },
      { once: true },
    );
    // There is no reliable "cancelled" event; the picker just never fires
    // change. The input is removed on the next scan either way.
    input.click();
  });
}

function manifest({ label, entries }) {
  const files = [];
  const skipped = [];
  for (const { path, file } of entries) {
    // A Mac writes a "._" shadow file beside every file on a FAT card. They
    // match the extension and hold no audio.
    if (AUDIO.test(file.name) && !file.name.startsWith("._")) {
      files.push({ path, bytes: file.size, night: nightOf(file.lastModified), file });
    } else {
      skipped.push(file.name);
    }
  }
  // Byte order, the way the server sorts a card's files.
  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));

  const byNight = new Map();
  for (const file of files) {
    const night = byNight.get(file.night) ?? { date: file.night, files: 0, bytes: 0 };
    night.files += 1;
    night.bytes += file.bytes;
    byNight.set(file.night, night);
  }

  const nights = [...byNight.values()].sort((a, b) => a.date.localeCompare(b.date));
  flagOddNights(nights);

  return {
    label,
    nights,
    files,
    fileCount: files.length,
    totalBytes: files.reduce((sum, f) => sum + f.bytes, 0),
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
