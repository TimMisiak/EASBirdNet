import { BaseElement, escapeHTML } from "./base-element.js";
import { controls, filters, panels, tables, typography } from "../shared-styles.js";
import { byteSize, clock, count, dateAtTime, longDate, percent, shortDate } from "../format.js";
import { analyzedSoFar, audioNote, fileChip, isMoving, reviewChip, statusChip } from "../upload-status.js";
import * as api from "../api.js";
import "./bs-chip.js";
import "./bs-progress-bar.js";
import "./bs-queue-note.js";

/**
 * <bs-admin-upload-detail reference="OWL-20260914-SR03"> -- one card, file by
 * file: where each is in being sent and analyzed, and, opened, everything
 * BirdNET heard in it, each linked to its own page for review. While the card is still moving the page looks again
 * every few seconds, keeping the filter and the open files as they were.
 *
 * Attribute: reference, the card.
 */
const POLL_MS = 5_000;

/**
 * The most detections in one file this page lists at once, which is the API's
 * own cap. A dawn chorus file can merge to hundreds; the rest are a page of
 * their own on the Detections tab.
 */
const HEARD_LIMIT = 500;

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
  /** Ids of the files whose detections are showing. */
  #open = new Set();
  /** fileId -> {status, detections, total, count}; count is the file's detectionCount when fetched. */
  #heard = new Map();
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
    this.#open.clear();
    this.#heard.clear();
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
      toggle: (el) => {
        const id = el.dataset.file;
        if (this.#open.has(id)) {
          this.#open.delete(id);
        } else {
          this.#open.add(id);
          this.#fetchHeard(this.#state.files.find((f) => f.id === id));
        }
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
      // A file that has been analyzed again since its detections were fetched
      // shows what it has now.
      for (const id of this.#open) this.#fetchHeard(files.find((f) => f.id === id));
    } catch (error) {
      if (reference !== this.reference) return;
      if (this.#state.status !== "ready") this.#state = { status: "error", upload: null, files: [], queue: null, error };
    }
    if (!this.isConnected) return;
    this.render();
    clearTimeout(this.#timer);
    if (this.#state.upload && isMoving(this.#state.upload)) {
      this.#timer = setTimeout(() => this.#load(), POLL_MS);
    }
  }

  async #fetchHeard(file) {
    if (!file || file.status !== "analyzed") return;
    const had = this.#heard.get(file.id);
    if (had && had.status !== "error" && had.count === file.detectionCount) return;

    this.#heard.set(file.id, {
      status: "loading", detections: had?.detections ?? [], total: had?.total ?? 0, count: file.detectionCount,
    });
    let next;
    try {
      const { detections, total } = await api.fetchFileDetections(this.reference, file.id, HEARD_LIMIT);
      next = { status: "ready", detections, total, count: file.detectionCount };
    } catch (error) {
      next = { status: "error", detections: [], total: 0, count: file.detectionCount, error };
    }
    this.#heard.set(file.id, next);
    if (this.isConnected) this.render();
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
        tr.file { cursor: pointer; }
        tr.file:hover { background: var(--bs-surface-sunk); }
        tr.file[aria-expanded="true"] { background: var(--bs-surface-sunk); }
        /* A path breaks only after a slash; on a narrow screen the table
           scrolls in its box rather than splitting a file name. */
        .file-cell { display: flex; align-items: baseline; min-width: 14rem; }
        .path { font-family: var(--bs-font-mono); font-size: 0.8125rem; }
        .disclose {
          flex: none;
          border: 0;
          background: none;
          padding: 0 var(--bs-space-2) 0 0;
          color: var(--bs-text-muted);
          font-size: 0.75rem;
          width: 1.25rem;
        }
        .why { display: block; margin-top: var(--bs-space-1); font-size: 0.78125rem; color: var(--bs-chip-attention-text); }
        /* Audio expiring is the policy working, not a problem: muted, not the
           attention colour .why uses. */
        .gone { display: block; margin-top: var(--bs-space-1); font-size: 0.78125rem; color: var(--bs-text-muted); }
        .nowrap { white-space: nowrap; }

        tr.heard { border-top: 0; background: var(--bs-surface-sunk); }
        tr.heard > td { padding: 0 0 var(--bs-space-4) 1.25rem; }
        .heard-box { background: var(--bs-surface); border: 1px solid var(--bs-border); padding: var(--bs-space-2) var(--bs-space-4); }
        .heard-box table td { padding-top: 0.5rem; padding-bottom: 0.5rem; font-size: 0.84375rem; }
        .heard-box .quiet { color: var(--bs-text-muted); font-size: 0.84375rem; padding: var(--bs-space-2) 0; }
        .sci { font-style: italic; color: var(--bs-text-muted); font-size: 0.8125rem; }
        .empty { color: var(--bs-text-muted); padding: var(--bs-space-5) 0; }
      </style>

      <a class="back" href="/admin/uploads">← All uploads</a>
      ${
        status === "loading"
          ? `<p class="empty">Loading the card…</p>`
          : status === "error"
            ? `<p class="empty">Couldn't load ${escapeHTML(this.reference)}: ${escapeHTML(error.message)}</p>`
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

      <dl class="stats">
        <div class="stat"><dt>Received</dt><dd>${count(upload.filesUploaded)} <small>of ${count(upload.fileCount)}</small></dd></div>
        <div class="stat"><dt>Analyzed</dt><dd>${count(upload.filesAnalyzed)}</dd></div>
        <div class="stat"><dt>Queued</dt><dd>${count(queued + analyzing)}${analyzing ? ` <small>${count(analyzing)} running</small>` : ""}</dd></div>
        <div class="stat"><dt>Failed</dt><dd>${count(upload.filesFailed)}</dd></div>
        <div class="stat"><dt>Detections</dt><dd>${count(upload.detectionCount)}</dd></div>
      </dl>
      ${
        received
          ? `<bs-progress-bar value="${percent(done, upload.fileCount)}" label="Files analyzed"></bs-progress-bar>`
          : `<bs-progress-bar value="${percent(upload.filesUploaded, upload.fileCount)}" label="Files received"></bs-progress-bar>`
      }
      <p class="settings">${this.#settings(upload)}</p>
      ${audioNote(upload) ? `<p class="retention">${escapeHTML(audioNote(upload))}</p>` : ""}

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
                       <th scope="col">Night</th>
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
    if (a.finishedAt) parts.push(`finished ${escapeHTML(dateAtTime(a.finishedAt))}`);
    return parts.join(" · ");
  }

  #fileRows(file, upload) {
    const chip = fileChip(file, upload);
    const open = this.#open.has(file.id);
    return `
      <tr class="file" data-action="toggle" data-file="${escapeHTML(file.id)}" aria-expanded="${open}">
        <td>
          <span class="file-cell">
            <button class="disclose" aria-expanded="${open}" aria-label="${open ? "Hide" : "Show"} detections in ${escapeHTML(file.path)}">${open ? "▾" : "▸"}</button>
            <span class="path">${escapeHTML(file.path).replaceAll("/", "/<wbr>")}</span>
          </span>
        </td>
        <td class="nowrap">${escapeHTML(shortDate(file.night))}</td>
        <td class="num">${escapeHTML(byteSize(file.bytes))}</td>
        <td>
          <bs-chip kind="${chip.kind}">${escapeHTML(chip.label)}</bs-chip>
          ${file.status === "failed" && file.statusDetail ? `<span class="why">${escapeHTML(file.statusDetail)}</span>` : ""}
          ${file.audioDeletedAt ? `<span class="gone">recording removed ${escapeHTML(shortDate(file.audioDeletedAt))}</span>` : ""}
        </td>
        <td class="num">${file.status === "analyzed" ? count(file.detectionCount) : "—"}</td>
      </tr>
      ${open ? `<tr class="heard"><td colspan="5"><div class="heard-box">${this.#heardIn(file, upload)}</div></td></tr>` : ""}
    `;
  }

  #heardIn(file, upload) {
    switch (file.status) {
      case "pending":
        return `<p class="quiet">This file hasn't been uploaded yet.</p>`;
      case "uploaded":
      case "analyzing":
        return upload.status === "processing"
          ? `<p class="quiet">BirdNET hasn't finished with this file yet.</p>`
          : `<p class="quiet">BirdNET starts once every file on the card is in.</p>`;
      case "failed":
        return `<p class="quiet">Not analyzed: ${escapeHTML(file.statusDetail || "no reason recorded")}.</p>`;
    }

    const heard = this.#heard.get(file.id);
    if (!heard || (heard.status === "loading" && heard.detections.length === 0)) {
      return `<p class="quiet">Loading detections…</p>`;
    }
    if (heard.status === "error") {
      return `<p class="quiet">Couldn't load the detections: ${escapeHTML(heard.error.message)}</p>`;
    }
    if (heard.detections.length === 0) {
      const floor = upload.analysis ? ` above ${Math.round(upload.analysis.minConfidence * 100)}% confidence` : "";
      return `<p class="quiet">BirdNET heard nothing${floor} in this file.</p>`;
    }
    return `
      <table>
        <thead>
          <tr>
            <th scope="col">In file</th>
            <th scope="col">Heard</th>
            <th scope="col">Species</th>
            <th scope="col" style="text-align: right;">Confidence</th>
            <th scope="col">Review</th>
          </tr>
        </thead>
        <tbody>
          ${heard.detections
            .map(
              (d) => `
            <tr>
              <td class="num nowrap" style="text-align: left;">${clock(d.startSec)}–${clock(d.endSec)}</td>
              <td class="nowrap">${escapeHTML(dateAtTime(d.detectedAt))}</td>
              <td><a href="${escapeHTML(`/admin/uploads/${encodeURIComponent(upload.reference)}/detections/${encodeURIComponent(d.id)}`)}">${escapeHTML(d.commonName)}</a>${
                d.scientificName && d.scientificName !== d.commonName
                  ? ` <span class="sci">${escapeHTML(d.scientificName)}</span>`
                  : ""
              }</td>
              <td class="num">${Math.round(d.confidence * 100)}%</td>
              <td>${reviewChipHTML(d.reviewStatus)}</td>
            </tr>`,
            )
            .join("")}
        </tbody>
      </table>
      ${
        heard.total > heard.detections.length
          ? `<p class="quiet">Showing the first ${count(heard.detections.length)} of ${count(heard.total)} detections in this file.</p>`
          : ""
      }
    `;
  }
}

function reviewChipHTML(status) {
  const { kind, label } = reviewChip(status);
  return `<bs-chip kind="${kind}">${escapeHTML(label)}</bs-chip>`;
}

customElements.define("bs-admin-upload-detail", AdminUploadDetail);
