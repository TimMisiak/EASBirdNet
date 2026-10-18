import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, filters, panels, tables, typography } from "../shared-styles.js";
import { byteSize, count, dateAtTime, longDate, percent, shortDate } from "../format.js";
import { analyzedSoFar, audioNote, fileChip, isMoving, statusChip } from "../upload-status.js";
import * as api from "../api.js";
import "./bs-chip.js";
import "./bs-progress-bar.js";
import "./bs-queue-note.js";

/**
 * <bs-admin-upload-detail reference="OWL-20260914-SR03"> -- one card, file by
 * file: where each is in being sent and analyzed, and how many detections
 * each yielded. Where the server runs Perch as a second step, each file says
 * how far Perch has got with it. While the card is still moving the page looks
 * again every few seconds, keeping the filter as it was.
 *
 * What was heard isn't listed here: the Detections tab is the one list of
 * detections, and this page links to it narrowed to the card
 * (/app/detections?card=…), and to Perch's on it as well (&model=perch).
 *
 * Attribute: reference, the card.
 */
const POLL_MS = 5_000;

const FILTERS = [
  { id: "all", label: "All files", match: () => true },
  { id: "queued", label: "Queued", match: (f) => f.status === "uploaded" || f.status === "pending" },
  { id: "analyzing", label: "Analyzing", match: (f) => f.status === "analyzing" },
  { id: "analyzed", label: "Analyzed", match: (f) => f.status === "analyzed" },
  { id: "failed", label: "Failed", match: (f) => f.status === "failed" },
  { id: "heard", label: "With detections", match: (f) => f.detectionCount > 0 },
];

class AdminUploadDetail extends BaseElement {
  static styles = [typography, controls, panels, tables, filters];
  static observedAttributes = ["reference"];

  #state = { status: "loading", upload: null, files: [], queue: null, error: null };
  #filter = "all";
  #timer = 0;

  connectedCallback() {
    super.connectedCallback();
    this.#load();
  }

  disconnectedCallback() {
    clearTimeout(this.#timer);
  }

  attributeChangedCallback() {
    if (!this.isConnected) return;
    this.#state = { status: "loading", upload: null, files: [], queue: null, error: null };
    this.render();
    this.#load();
  }

  get reference() {
    return this.getAttribute("reference") ?? "";
  }

  get actions() {
    return {
      filter: (el) => {
        this.#filter = el.dataset.filter;
        this.render();
      },
    };
  }

  async #load() {
    const reference = this.reference;
    try {
      const { upload, files, queue } = await api.fetchCardFiles(reference);
      if (reference !== this.reference) return;
      this.#state = { status: "ready", upload, files, queue, error: null };
    } catch (error) {
      if (reference !== this.reference) return;
      // A card that was on the page and has gone was deleted, most likely
      // while it said so.
      if (this.#state.status === "ready" && error.status === 404) {
        this.#state = { status: "deleted", upload: null, files: [], queue: null, error: null };
      } else if (this.#state.status !== "ready") {
        this.#state = { status: "error", upload: null, files: [], queue: null, error };
      }
    }
    if (!this.isConnected) return;
    this.render();
    clearTimeout(this.#timer);
    if (this.#state.upload && isMoving(this.#state.upload)) {
      this.#timer = setTimeout(() => this.#load(), POLL_MS);
    }
  }

  render() {
    const { status, upload, files, queue, error } = this.#state;

    this.shadowRoot.innerHTML = `
      <style>
        .back { display: inline-block; font-size: 0.875rem; margin-bottom: var(--bs-space-4); }
        .head {
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: var(--bs-space-4);
          flex-wrap: wrap;
        }
        .ref { font-family: var(--bs-font-mono); font-size: 1.25rem; letter-spacing: 0.02em; }
        .sub { margin-top: var(--bs-space-2); font-size: 0.90625rem; color: var(--bs-text-body); }
        .note-line { margin-top: var(--bs-space-2); font-size: 0.84375rem; color: var(--bs-text-muted); }

        .stats {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(8.5rem, 1fr));
          gap: var(--bs-space-4);
          margin: var(--bs-space-6) 0 var(--bs-space-4);
          padding: 0;
        }
        .stat { border-top: 2px solid var(--bs-text); padding-top: var(--bs-space-2); margin: 0; }
        .stat dt { font-family: var(--bs-font-mono); font-size: 0.65625rem; letter-spacing: 0.14em; text-transform: uppercase; color: var(--bs-text-muted); }
        .stat dd { margin: var(--bs-space-1) 0 0; font-family: var(--bs-font-display); font-size: 1.625rem; }
        .stat dd small { font-family: var(--bs-font); font-size: 0.8125rem; color: var(--bs-text-muted); }
        .settings { font-size: 0.8125rem; color: var(--bs-text-muted); margin: var(--bs-space-3) 0 var(--bs-space-2); }
        .retention { font-size: 0.8125rem; color: var(--bs-text-muted); margin: 0 0 var(--bs-space-6); }

        .filters { gap: var(--bs-space-2); }

        th { padding-top: 0; }
        td { padding-top: 0.75rem; padding-bottom: 0.75rem; font-size: 0.875rem; vertical-align: middle; }
        /* A path breaks only after a slash; on a narrow screen the table
           scrolls in its box rather than splitting a file name. */
        .path { display: inline-block; min-width: 14rem; font-family: var(--bs-font-mono); font-size: 0.8125rem; }
        .why { display: block; margin-top: var(--bs-space-1); font-size: 0.78125rem; color: var(--bs-chip-attention-text); }
        /* Audio expiring is the policy working, not a problem: muted, not the
           attention colour .why uses. */
        .gone { display: block; margin-top: var(--bs-space-1); font-size: 0.78125rem; color: var(--bs-text-muted); }
        .nowrap { white-space: nowrap; }
        .perch-line { display: block; margin-top: var(--bs-space-1); font-size: 0.78125rem; color: var(--bs-text-muted); }
        .stat dd a { color: inherit; }
        .heard { margin: var(--bs-space-4) 0 var(--bs-space-6); font-size: 0.875rem; }
        .heard a + a { margin-left: var(--bs-space-4); }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
      </style>

      <a class="back" href="/admin/uploads">← All uploads</a>
      ${
        status === "loading"
          ? `<p class="empty">Loading the card…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load ${escapeHTML(this.reference)}: ${escapeHTML(error.message)}</p>`
            : status === "deleted"
              ? `<p class="empty">${escapeHTML(this.reference)} has been deleted, with its recordings, clips and detections.</p>`
              : this.#card(upload, files)
      }
    `;

    // Only a card that is waiting for BirdNET has anything to learn from the
    // queue's state; #card leaves the element out otherwise.
    const note = this.$("bs-queue-note");
    if (note) note.queue = queue;
  }

  #card(upload, files) {
    const chip = statusChip(upload);
    const received = upload.status !== "in_progress" && upload.status !== "interrupted";
    const done = analyzedSoFar(upload);
    const queued = files.filter((f) => f.status === "uploaded").length;
    const analyzing = files.filter((f) => f.status === "analyzing").length;
    const active = FILTERS.find((f) => f.id === this.#filter) ?? FILTERS[0];
    const shown = files.filter(active.match);
    // Perch's step, on the files it was queued for: only there when the
    // server ran Perch over this card.
    const perched = files.filter((f) => f.perch);
    const perchDone = perched.filter((f) => f.perch.status === "analyzed" || f.perch.status === "failed").length;

    return `
      <div class="head">
        <h1 class="ref">${escapeHTML(upload.reference)}</h1>
        <bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip>
      </div>
      <p class="sub">
        ${escapeHTML(upload.stationName)} · ${escapeHTML(upload.volunteerName)} ·
        pulled ${escapeHTML(longDate(upload.pulledOn))}
      </p>
      ${upload.notes ? `<p class="note-line">“${escapeHTML(upload.notes)}”</p>` : ""}
      ${upload.status === "processing" ? `<bs-queue-note></bs-queue-note>` : ""}
      ${
        upload.status === "deleting"
          ? `<p class="note-line">This card is being deleted: its recordings, clips and detections are being removed. This page says so when it's gone.</p>`
          : ""
      }

      <dl class="stats">
        <div class="stat"><dt>Received</dt><dd>${count(upload.filesUploaded)} <small>of ${count(upload.fileCount)}</small></dd></div>
        <div class="stat"><dt>Analyzed</dt><dd>${count(upload.filesAnalyzed)}</dd></div>
        <div class="stat"><dt>Queued</dt><dd>${count(queued + analyzing)}${analyzing ? ` <small>${count(analyzing)} running</small>` : ""}</dd></div>
        <div class="stat"><dt>Failed</dt><dd>${count(upload.filesFailed)}</dd></div>
        <div class="stat"><dt>Detections</dt><dd>${linked(upload.detectionCount, detectionsHref(upload))}</dd></div>
        ${
          perched.length
            ? `<div class="stat"><dt>Perch</dt><dd>${count(perchDone)} <small>of ${count(perched.length)} files</small></dd></div>
               <div class="stat"><dt>Perch detections</dt><dd>${linked(upload.perchDetectionCount ?? 0, detectionsHref(upload, "perch"))}</dd></div>`
            : ""
        }
      </dl>
      ${
        received
          ? `<bs-progress-bar value="${percent(done, upload.fileCount)}" label="Files analyzed"></bs-progress-bar>`
          : `<bs-progress-bar value="${percent(upload.filesUploaded, upload.fileCount)}" label="Files received"></bs-progress-bar>`
      }
      <p class="settings">${this.#settings(upload)}</p>
      ${audioNote(upload) ? `<p class="retention">${escapeHTML(audioNote(upload))}</p>` : ""}
      ${this.#heardLinks(upload)}

      <div class="filters" role="group" aria-label="Show files">
        ${FILTERS.map((f) => {
          const n = files.filter(f.match).length;
          return `<button class="filter" data-action="filter" data-filter="${f.id}"
                          aria-pressed="${f.id === active.id}">${f.label} · ${count(n)}</button>`;
        }).join("")}
      </div>

      ${
        files.length === 0
          ? `<p class="empty">No files are on record for this card.</p>`
          : shown.length === 0
            ? `<p class="empty">No files match that filter.</p>`
            : `<div class="table-scroll">
                 <table>
                   <thead>
                     <tr>
                       <th scope="col">File</th>
                       <th scope="col">Recorded</th>
                       <th scope="col" style="text-align: right;">Size</th>
                       <th scope="col">Status</th>
                       <th scope="col" style="text-align: right;">Detections</th>
                     </tr>
                   </thead>
                   <tbody>${shown.map((f) => this.#fileRows(f, upload)).join("")}</tbody>
                 </table>
               </div>`
      }
    `;
  }

  #settings(upload) {
    const a = upload.analysis;
    if (!a) {
      return upload.status === "processing"
        ? "Waiting for BirdNET to start on this card."
        : upload.status === "in_progress" || upload.status === "interrupted"
          ? "BirdNET starts once every file is in."
          : "";
    }
    const parts = [
      a.model ? escapeHTML(a.model) : "BirdNET",
      `detections from ${Math.round(a.minConfidence * 100)}% confidence`,
      `started ${escapeHTML(dateAtTime(a.startedAt))}`,
    ];
    if (a.perchModel) parts.push(`then ${escapeHTML(a.perchModel)}`);
    if (a.finishedAt) parts.push(`finished ${escapeHTML(dateAtTime(a.finishedAt))}`);
    return parts.join(" · ");
  }

  /** Where to review what was heard on the card: the Detections tab, narrowed to it. */
  #heardLinks(upload) {
    const birdnet = upload.detectionCount > 0;
    const perch = (upload.perchDetectionCount ?? 0) > 0;
    if (!birdnet && !perch) return "";
    return `
      <p class="heard">
        ${birdnet ? `<a href="${escapeHTML(detectionsHref(upload))}">Review this card's detections →</a>` : ""}
        ${perch ? `<a href="${escapeHTML(detectionsHref(upload, "perch"))}">Perch's detections →</a>` : ""}
      </p>`;
  }

  #fileRows(file, upload) {
    const chip = fileChip(file, upload);
    return `
      <tr>
        <td><span class="path">${escapeHTML(file.path).replaceAll("/", "/<wbr>")}</span></td>
        <td class="nowrap">${file.recordedAt ? escapeHTML(dateAtTime(file.recordedAt)) : "—"}</td>
        <td class="num">${escapeHTML(byteSize(file.bytes))}</td>
        <td>
          <bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip>
          ${file.status === "failed" && file.statusDetail ? `<span class="why">${escapeHTML(file.statusDetail)}</span>` : ""}
          ${file.audioDeletedAt ? `<span class="gone">recording removed ${escapeHTML(shortDate(file.audioDeletedAt))}</span>` : ""}
          ${perchLine(file.perch)}
        </td>
        <td class="num">${file.status === "analyzed" ? count(file.detectionCount) : "—"}</td>
      </tr>
    `;
  }
}

/** The Detections tab narrowed to a card, and to one model's detections on it if not BirdNET's. */
function detectionsHref(upload, model = "") {
  const params = new URLSearchParams({ card: upload.reference });
  if (model) params.set("model", model);
  return `/app/detections?${params}`;
}

/** A count, linked to where its detections are listed once there are any. */
function linked(n, href) {
  return n > 0 ? `<a href="${escapeHTML(href)}">${count(n)}</a>` : count(n);
}

/** Where Perch's second opinion on a file stands, as a line under its status, or "". */
function perchLine(perch) {
  if (!perch) return "";
  switch (perch.status) {
    case "queued":
      return `<span class="perch-line">Perch: queued</span>`;
    case "analyzing":
      return `<span class="perch-line">Perch: analyzing</span>`;
    case "analyzed":
      return `<span class="perch-line">Perch: ${count(perch.detectionCount)} ${perch.detectionCount === 1 ? "detection" : "detections"}</span>`;
    case "failed":
      return `<span class="why">Perch failed: ${escapeHTML(perch.statusDetail || "no reason recorded")}</span>`;
    default:
      return "";
  }
}

customElements.define("bs-admin-upload-detail", AdminUploadDetail);
