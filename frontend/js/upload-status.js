// How a card's status reads on screen. The server stores a machine value and,
// when a coordinator needs to know why, a short detail; the wording and the
// chip colour are a presentation decision and live here.

import { count, dateAtTime, longDate } from "./format.js";

const LABELS = {
  in_progress: "In progress",
  interrupted: "Unfinished",
  processing: "Processing",
  in_review: "In review",
  needs_attention: "Needs attention",
  results_sent: "Results sent",
};

const KINDS = {
  in_progress: "progress",
  interrupted: "progress",
  processing: "processing",
  in_review: "processing",
  needs_attention: "attention",
  results_sent: "done",
};

/** @returns {{kind: string, label: string}} for <bs-chip>. */
export function statusChip(upload) {
  const kind = KINDS[upload.status] ?? "neutral";
  let label = upload.statusDetail || LABELS[upload.status] || upload.status;

  // While a card is moving, the count is the status -- "In progress" alone
  // doesn't tell a volunteer whether to go make tea or go to bed.
  if (upload.status === "in_progress" || upload.status === "interrupted") {
    label = `${LABELS[upload.status]} · ${count(upload.filesUploaded)}/${count(upload.fileCount)}`;
  }
  // Same once it's in: BirdNET takes hours over a full card.
  if (upload.status === "processing") {
    label = `${LABELS.processing} · ${count(analyzedSoFar(upload))}/${count(upload.fileCount)}`;
  }
  return { kind, label };
}

/** Files BirdNET is done with, whether or not it could read them. */
export const analyzedSoFar = (upload) => (upload.filesAnalyzed ?? 0) + (upload.filesFailed ?? 0);

/** True while the server is still moving a card along, so a page showing it should look again. */
export const isMoving = (upload) => isUnfinished(upload) || upload.status === "processing";

/**
 * How one file on a card reads on screen. An uploaded file on a card that is
 * processing is waiting its turn for BirdNET.
 * @returns {{kind: string, label: string}} for <bs-chip>.
 */
export function fileChip(file, upload) {
  switch (file.status) {
    case "pending":
      return { kind: "neutral", label: "Not uploaded" };
    case "uploaded":
      return upload.status === "processing"
        ? { kind: "neutral", label: "Queued" }
        : { kind: "neutral", label: "Uploaded" };
    case "analyzing":
      return { kind: "progress", label: "Analyzing" };
    case "analyzed":
      return { kind: "done", label: "Analyzed" };
    case "failed":
      return { kind: "attention", label: "Failed" };
    default:
      return { kind: "neutral", label: file.status };
  }
}

/**
 * What has become of a card's original recordings. They are kept for a month
 * after the card is received and then removed; the detections BirdNET found in
 * them, and the clips of those, are kept for good. Nothing plays an original,
 * so this is a coordinator's record of what is still there to re-analyze.
 *
 * Retention off on the server sends neither date, and the card says nothing.
 * @returns {string} a sentence, or "" when there is nothing to say.
 */
export function audioNote(upload) {
  if (upload.audioDeletedAt) {
    return `Original recordings removed ${longDate(upload.audioDeletedAt)}. Detections and their clips are kept.`;
  }
  if (upload.audioExpiresAt) {
    return `Original recordings kept until ${longDate(upload.audioExpiresAt)}. Detections and their clips are kept.`;
  }
  return "";
}

// Why nothing is being analyzed, by the state internal/analysis reports. A
// state not listed here -- "ready", or "starting" while the server's first
// BirdNET check runs -- has nothing to say.
const QUEUE_NOTES = {
  unavailable: {
    headline: "BirdNET isn’t running on this server.",
    what: "Received cards wait in Processing until it is. Nothing is lost: analysis starts on its own once the server can run it, and picks up where it left off.",
  },
  failing: {
    headline: "Analysis stopped on an error.",
    what: "The queue is retrying it. Cards may sit in Processing until it gets through.",
  },
  off: {
    headline: "This server runs no analysis.",
    what: "Received cards stay in Processing.",
  },
};

/**
 * Why cards aren’t moving, for the coordinator’s screens. Every card reaches
 * BirdNET through the one server process, so when that can’t run, every card
 * in Processing is waiting on the same thing -- and the screens that list them
 * are where a coordinator should find that out, rather than in container logs.
 *
 * @returns {{headline: string, what: string, detail: string} | null} null when
 *   analysis is running, which is the usual answer.
 */
export function queueNote(queue) {
  const note = QUEUE_NOTES[queue?.state];
  if (!note) return null;
  // Separated rather than punctuated: dateAtTime ends in "a.m." often enough
  // that a full stop after it reads as a typo.
  const detail = [queue.since ? `Since ${dateAtTime(queue.since)}` : "", queue.detail ?? ""]
    .filter(Boolean)
    .join(" · ");
  return { ...note, detail };
}

/** True when the volunteer still has work to do on this card. */
export const isUnfinished = (upload) =>
  upload.status === "in_progress" || upload.status === "interrupted";

const REVIEWS = {
  unreviewed: { kind: "neutral", label: "Unreviewed" },
  confirmed: { kind: "done", label: "Confirmed" },
  rejected: { kind: "attention", label: "Discarded" },
};

/**
 * How a detection's review reads on screen. "rejected" is stored, but the
 * button that sets it says Discard, so that is what the chip says too.
 * @returns {{kind: string, label: string}} for <bs-chip>.
 */
export const reviewChip = (status) => REVIEWS[status] ?? { kind: "neutral", label: status };
