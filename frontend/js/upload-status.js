// How a card's status reads on screen. The server stores a machine value and,
// when a coordinator needs to know why, a short detail; the wording and the
// chip colour are a presentation decision and live here.

import { count } from "./format.js";

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
