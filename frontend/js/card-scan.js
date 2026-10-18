// Reading an SD card in the browser.
//
// The card is never copied to the volunteer's disk: we ask for the folder, walk
// it, and keep file handles. What comes back is a manifest -- which days the
// card recorded on, how many files, how big -- which is what the "here's what's
// on the card" screen is built on, and the list of files the upload sends.

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
 * {path, bytes, date, file}: the path from the card's root with forward
 * slashes, the date it was recorded on (for the check screen; the server
 * reads the time itself, from the name), and the File to send.
 *
 * `onWorking` is called when the scan starts on work that finishes: at once
 * where the picker reports a cancellation, and only once the files are in
 * where it doesn't (see `viaInput`). A caller that holds its screen while the
 * card is read holds it from there, so a picker that never answers leaves the
 * page as it was rather than stuck.
 * @returns {Promise<{label: string, days: object[], files: object[], fileCount: number, totalBytes: number, skipped: string[]}>}
 */
export async function scanCard({ onWorking } = {}) {
  if (window.showDirectoryPicker) {
    onWorking?.();
    return manifest(await viaDirectoryPicker());
  }
  const found = await viaInput();
  onWorking?.();
  return manifest(found);
}

// The modern path: a real directory handle, so the card is walked where it sits
// and nothing is copied to the volunteer's disk. The handle itself is dropped
// once the manifest is built, so a resume asks for the folder again exactly
// like the input fallback does -- `id` at least reopens the picker where it
// left off. Keeping it would mean storing the handle in IndexedDB and
// re-requesting read permission on the way back.
async function viaDirectoryPicker() {
  let handle;
  try {
    handle = await window.showDirectoryPicker({ id: "birdsense-card", mode: "read" });
  } catch (error) {
    // Only an AbortError is "they closed the picker", which the screen shows as
    // nothing at all. A SecurityError or a NotAllowedError is the browser
    // refusing, and a volunteer who sees no message has no idea why nothing
    // happened -- so those travel on to be reported.
    if (error?.name !== "AbortError") throw error;
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

/** The input the volunteer is answering, if any: one card is chosen at a time. */
let pending = null;

// The fallback path: <input webkitdirectory>, which every current browser
// supports. It reads the whole folder into File objects up front. This is the
// path every Firefox and Safari volunteer takes, so closing the picker has to
// settle the promise: "change" for a folder, "cancel" for a closed picker.
//
// Firefox asks a second question after the folder picker -- "Are you sure you
// want to upload all files from X?" -- and when the answer is no it tells the
// page nothing at all: no "change", no "cancel", and the prompt is tab-modal,
// so the window never loses focus either. There is no signal to turn that into
// a cancellation, so such a scan stays pending for good. What keeps the page
// usable is that nothing waits on it (see `scanCard`) and that choosing the
// card again abandons it.
function viaInput() {
  pending?.();
  return new Promise((resolve, reject) => {
    const input = document.createElement("input");
    input.type = "file";
    input.webkitdirectory = true;
    input.multiple = true;
    input.style.display = "none";
    document.body.append(input);

    // Whichever event arrives first takes the promise with it and takes the
    // input out of the document; the other is then a no-op.
    let settled = false;
    const settle = (finish) => () => {
      if (settled) return;
      settled = true;
      pending = null;
      input.remove();
      finish();
    };
    const giveUp = settle(() => reject(new CancelledError()));
    pending = giveUp;

    input.addEventListener(
      "change",
      settle(() => {
        const files = [...input.files];
        if (!files.length) return reject(new CancelledError());
        // Relative paths start with the folder that was picked; the card's
        // paths start inside it.
        const [label] = files[0].webkitRelativePath.split("/");
        const entries = files.map((file) => ({
          path: file.webkitRelativePath.split("/").slice(1).join("/") || file.name,
          file,
        }));
        resolve({ label: label || "SD card", entries });
      }),
    );
    // <input type=file> has fired "cancel" since Firefox 91 and Safari 16.4.
    // Somewhere older leaves the promise pending, which is what it did here
    // for every browser before this handler existed.
    input.addEventListener("cancel", giveUp);
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
      files.push({ path, bytes: file.size, date: dateOf(file), file });
    } else {
      skipped.push(file.name);
    }
  }
  // Byte order, the way the server sorts a card's files.
  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));

  const byDay = new Map();
  for (const file of files) {
    const day = byDay.get(file.date) ?? { date: file.date, files: 0, bytes: 0 };
    day.files += 1;
    day.bytes += file.bytes;
    byDay.set(file.date, day);
  }

  const days = [...byDay.values()].sort((a, b) => a.date.localeCompare(b.date));
  flagOddDays(days);

  return {
    label,
    days,
    files,
    fileCount: files.length,
    totalBytes: files.reduce((sum, f) => sum + f.bytes, 0),
    skipped,
  };
}

/**
 * Recorders put the start of each recording in the file name, as local time:
 * "Marymoor_20260723_160624(-0700).wav" began at 16:06:24 on July 23. The
 * "(-0700)" is the UTC offset, which a date as the recorder wrote it doesn't
 * need.
 */
const NAMED_START = /(?<!\d)(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})(?!\d)/;

/**
 * The date a file was recorded on, as the recorder's clock had it.
 *
 * The time comes from the file name. A file's modified time is when it was
 * last copied, not recorded, so it is only the fallback for a name without one.
 */
function dateOf(file) {
  const named = startFromName(file.name);
  if (named !== null) {
    // Wall-clock arithmetic in UTC, so the browser's own timezone stays out of it.
    return new Date(named).toISOString().slice(0, 10);
  }
  const t = new Date(file.lastModified);
  const pad = (n) => String(n).padStart(2, "0");
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())}`;
}

/** The recording's local start as if it were UTC, in ms, or null if the name has none. */
function startFromName(name) {
  const match = NAMED_START.exec(name);
  if (!match) return null;
  const [year, month, day, hour, minute, second] = match.slice(1).map(Number);
  const ms = Date.UTC(year, month - 1, day, hour, minute, second);
  const t = new Date(ms);
  // Date.UTC rolls "20260231" over into March; a name like that isn't a date.
  const real =
    t.getUTCMonth() === month - 1 && t.getUTCDate() === day &&
    t.getUTCHours() === hour && t.getUTCMinutes() === minute && t.getUTCSeconds() === second;
  return real ? ms : null;
}

/**
 * Mark the days worth a second look. A day with far fewer files than the
 * others usually means flat batteries or a knocked-over recorder. The first
 * and last days are partial whatever happened -- the card went in one day and
 * came out another -- so they are marked as that instead.
 */
function flagOddDays(days) {
  if (days.length < 2) return;
  days[0].flag = "first";
  days[days.length - 1].flag = "last";
  const middle = days.slice(1, -1);
  if (middle.length < 3) return;
  const counts = middle.map((d) => d.files).sort((a, b) => a - b);
  const typical = counts[Math.floor(counts.length / 2)];
  for (const day of middle) {
    if (day.files < typical * 0.75) day.flag = "short";
  }
}
