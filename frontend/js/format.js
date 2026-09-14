// Formatting shared across components. The program's audience is volunteers,
// not engineers: sizes are decimal GB (what a card claims on the box), times
// are spelled out, and counts are grouped.

const numbers = new Intl.NumberFormat("en-US");

export const count = (n) => numbers.format(n ?? 0);

/** Decimal gigabytes, the unit the card and the ISP both quote. */
export function gigabytes(bytes, digits = 1) {
  return `${((bytes ?? 0) / 1e9).toFixed(digits)} GB`;
}

/** "383 MB" or "1.2 GB" -- one file, where decimal GB alone would read "0.4 GB". */
export function fileSize(bytes) {
  const b = bytes ?? 0;
  if (b >= 1e9) return gigabytes(b);
  return b >= 1e7 ? `${Math.round(b / 1e6)} MB` : `${(b / 1e6).toFixed(1)} MB`;
}

/** "42 Mb/s" -- a link speed in megabits, the unit the ISP quotes. */
export function megabits(bytesPerSecond) {
  const mbps = ((bytesPerSecond ?? 0) * 8) / 1e6;
  return `${mbps >= 10 ? Math.round(mbps) : mbps.toFixed(1)} Mb/s`;
}

export function percent(part, whole) {
  if (!whole) return 0;
  return Math.min(100, Math.max(0, (part / whole) * 100));
}

const dateOnly = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  year: "numeric",
});
const dayMonth = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric" });
const dateTime = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

/** "Sep 14, 2026 · 06:40" -- when a page says how fresh it is. */
export function stamp(value) {
  const d = toDate(value);
  if (!d) return "";
  const time = new Intl.DateTimeFormat("en-US", {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(d);
  return `${dateOnly.format(d)} · ${time}`;
}

/** "Sep 7, 2026" from an ISO date or datetime. */
export function longDate(value) {
  const d = toDate(value);
  return d ? dateOnly.format(d) : "";
}

/** "Sep 7" -- for the night-by-night lists, where the year is obvious. */
export function shortDate(value) {
  const d = toDate(value);
  return d ? dayMonth.format(d) : "";
}

/** "Sep 13, 3:12 a.m." -- owls are a small-hours business. */
export function dateAtTime(value) {
  const d = toDate(value);
  if (!d) return "";
  return dateTime.format(d).replace(/\bAM\b/, "a.m.").replace(/\bPM\b/, "p.m.");
}

/** "Aug 24 – Sep 6" for a card's span of nights. */
export function nightRange(nights) {
  if (!nights?.length) return "";
  const first = shortDate(nights[0].date);
  const last = shortDate(nights[nights.length - 1].date);
  return first === last ? first : `${first} – ${last}`;
}

/** "about 40 minutes left", rounded the way a person would say it. */
export function minutesLeft(minutes) {
  if (!Number.isFinite(minutes) || minutes <= 0) return "almost done";
  if (minutes < 60) return `about ${Math.max(1, Math.round(minutes))} minutes left`;
  const hours = Math.floor(minutes / 60);
  const rest = Math.round(minutes % 60);
  return rest ? `about ${hours} h ${rest} m left` : `about ${hours} hours left`;
}

/** "2 h 40 m" -- an estimate on the check screen, not a countdown. */
export function duration(minutes) {
  if (!Number.isFinite(minutes) || minutes < 1) return "under a minute";
  const hours = Math.floor(minutes / 60);
  const rest = Math.round(minutes % 60);
  return hours ? `${hours} h ${rest} m` : `${rest} m`;
}

function toDate(value) {
  if (!value) return null;
  // A bare YYYY-MM-DD parses as UTC midnight, which shows as the day before in
  // Pacific time. Pin it to local noon so the date reads as written.
  const d = /^\d{4}-\d{2}-\d{2}$/.test(value) ? new Date(`${value}T12:00:00`) : new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}
