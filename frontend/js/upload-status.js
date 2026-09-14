// How a card's status reads on screen. The server stores a machine value and,
// when a coordinator needs to know why, a short detail; the wording and the
// chip colour are a presentation decision and live here.

import { count } from "./format.js";

const LABELS = {
  in_progress: "In progress",
  interrupted: "Unfinished",
  processing: "Processing",
  needs_attention: "Needs attention",
  results_sent: "Results sent",
};

const KINDS = {
  in_progress: "progress",
  interrupted: "progress",
  processing: "processing",
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
  return { kind, label };
}

/** True when the volunteer still has work to do on this card. */
export const isUnfinished = (upload) =>
  upload.status === "in_progress" || upload.status === "interrupted";
