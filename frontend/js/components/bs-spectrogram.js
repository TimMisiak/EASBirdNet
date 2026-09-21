import { controls, reset } from "../shared-styles.js";
import { clock } from "../format.js";
import { escapeHTML } from "./base-element.js";

/**
 * <bs-spectrogram src="/api/v1/.../clip" clip-start="731"> -- a clip to play,
 * drawn as a spectrogram with a playhead. The browser draws it from the audio
 * itself: Web Audio decodes the WAV and a short FFT below does the rest, so the
 * clip is all that is stored.
 *
 * Attribute: clip-start, the seconds into the recording where the clip begins,
 * for the time axis.
 *
 * The plot is the transport: it is a slider, so the playhead moves by click or
 * by key (arrows a second, Page a five, Home and End the ends). It swallows the
 * arrows it uses, because the page around it binds the same two to stepping
 * through the list -- a focused player has to mean the player.
 *
 * Property: marks, what was heard, as [{label, spans: [{startSec, endSec}]}] in
 * seconds into the recording. Each mark gets its own strip above the plot, in
 * the next --bs-species-* colour, and a line in the legend.
 *
 * Extends HTMLElement rather than BaseElement: the playhead moves many times a
 * second, and rewriting the shadow root would stop the audio.
 */

/** The top of the plot. Birds rarely sing above it; owls call far below. */
const TOP_HZ = 12_000;
/** Frequency bins about this wide: ~20 ms windows, the usual trade for birdsong. */
const BIN_HZ = 46;
/** Enough columns for a wide screen at 2x; more is FFT time nobody sees. */
const MAX_COLUMNS = 2400;
/** How many --bs-species-* colours there are; marks past it reuse them. */
const SPECIES_COLOURS = 6;

class Spectrogram extends HTMLElement {
  static observedAttributes = ["src", "clip-start"];

  #audio;
  #button;
  #time;
  #status;
  #stage;
  #canvas;
  #playhead;
  #bands;
  #legend;
  #marks = [];
  #ticks;
  #freq;
  /** The spectrogram at one pixel per column and bin, scaled onto #canvas. */
  #image = null;
  #topHz = TOP_HZ;
  #duration = 0;
  #loading = "";
  #url = "";
  #frame = 0;
  #resize = new ResizeObserver(() => this.#draw());

  constructor() {
    super();
    this.attachShadow({ mode: "open" });
    this.shadowRoot.adoptedStyleSheets = [reset, controls];
    this.shadowRoot.innerHTML = `
      <style>
        :host { display: block; --plot-height: 15rem; --band: 0.5rem; --band-gap: 2px; }
        .bar { display: flex; align-items: center; gap: var(--bs-space-3); margin-bottom: var(--bs-space-3); flex-wrap: wrap; }
        .play { min-width: 6.5rem; }
        .time { font-family: var(--bs-font-mono); font-size: 0.8125rem; color: var(--bs-text-soft); }
        .status { font-size: 0.8125rem; color: var(--bs-text-muted); }

        /* --lanes is how many strips are stacked above the plot; set from the marks. */
        .plot {
          display: grid;
          grid-template-columns: 3.5rem minmax(0, 1fr);
          --strips: calc(var(--lanes, 1) * var(--band) + (var(--lanes, 1) - 1) * var(--band-gap));
        }
        .freq { position: relative; height: var(--plot-height); margin-top: var(--strips); }
        .freq span, .ticks span {
          position: absolute;
          font-family: var(--bs-font-mono);
          font-size: 0.6875rem;
          color: var(--bs-text-muted);
          white-space: nowrap;
        }
        .freq span { right: var(--bs-space-2); transform: translateY(50%); }
        .freq span:first-child { transform: none; }

        .area { position: relative; padding-top: var(--strips); }
        .stage {
          position: relative;
          height: var(--plot-height);
          background: var(--bs-surface);
          border: 1px solid var(--bs-border);
          cursor: pointer;
          overflow: hidden;
        }
        /* The ring is on the stage itself, which has no padding to lose it in. */
        .stage:focus-visible { outline-offset: -2px; }
        canvas { display: block; width: 100%; height: 100%; }
        .playhead {
          position: absolute;
          top: 0;
          bottom: 0;
          left: 0;
          width: 2px;
          margin-left: -1px;
          background: var(--bs-amber-ink);
          pointer-events: none;
        }
        /* What was heard: a solid strip above the plot in its lane, dashed edges down it. */
        .band {
          position: absolute;
          top: calc(var(--lane) * (var(--band) + var(--band-gap)));
          bottom: 0;
          border-top: var(--band) solid var(--mark);
          border-left: 1px dashed var(--mark-ink);
          border-right: 1px dashed var(--mark-ink);
          pointer-events: none;
        }
        .ticks { grid-column: 2; position: relative; height: 1.5rem; }
        .ticks span { top: var(--bs-space-1); transform: translateX(-50%); }
        .ticks span::before {
          content: "";
          position: absolute;
          left: 50%;
          top: calc(-1 * var(--bs-space-1));
          height: 0.25rem;
          border-left: 1px solid var(--bs-border-strong);
        }
        .legend {
          grid-column: 2;
          display: flex;
          align-items: center;
          flex-wrap: wrap;
          gap: var(--bs-space-2) var(--bs-space-4);
          font-size: 0.78125rem;
          color: var(--bs-text-muted);
        }
        .legend .key { display: inline-flex; align-items: center; gap: var(--bs-space-2); color: var(--bs-text-soft); }
        .swatch { width: 1.25rem; height: var(--band); background: var(--mark); }
      </style>

      <div class="bar">
        <button class="btn btn--forest btn--small play" type="button" disabled>▶ Play</button>
        <span class="time">0:00</span>
        <span class="status" role="status"></span>
      </div>
      <div class="plot">
        <div class="freq" aria-hidden="true"></div>
        <div class="area">
          <div class="stage" tabindex="0" role="slider" aria-label="Playback position"
               aria-describedby="picture" aria-valuemin="0" aria-valuemax="0" aria-valuenow="0">
            <canvas id="picture" role="img"></canvas>
            <div class="playhead" hidden></div>
          </div>
          <div class="bands" aria-hidden="true"></div>
        </div>
        <div class="ticks" aria-hidden="true"></div>
        <p class="legend" hidden></p>
      </div>
      <audio preload="auto"></audio>
    `;

    const $ = (selector) => this.shadowRoot.querySelector(selector);
    this.#audio = $("audio");
    this.#button = $(".play");
    this.#time = $(".time");
    this.#status = $(".status");
    this.#stage = $(".stage");
    this.#canvas = $("canvas");
    this.#playhead = $(".playhead");
    this.#bands = $(".bands");
    this.#legend = $(".legend");
    this.#ticks = $(".ticks");
    this.#freq = $(".freq");

    this.#button.addEventListener("click", () => this.toggle());
    this.#audio.addEventListener("play", () => {
      this.#button.textContent = "❚❚ Pause";
      this.#tick();
    });
    this.#audio.addEventListener("pause", () => {
      this.#button.textContent = "▶ Play";
      this.#tick();
    });
    this.#audio.addEventListener("seeked", () => this.#tick());
    this.#audio.addEventListener("loadedmetadata", () => {
      if (!this.#duration) this.#duration = this.#audio.duration;
      this.#layout();
    });
    this.#stage.addEventListener("click", (event) => {
      if (!this.#duration) return;
      const box = this.#stage.getBoundingClientRect();
      this.#seek(Math.min(1, Math.max(0, (event.clientX - box.left) / box.width)) * this.#duration);
    });
    this.#stage.addEventListener("keydown", (event) => {
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      const to = this.#keySeek(event.key);
      if (to === null) return;
      // Arrow keys step the detections list on the page around this one, and
      // they scroll it. Neither is what a focused player means by an arrow.
      event.preventDefault();
      event.stopPropagation();
      if (this.#duration) this.#seek(to);
    });
  }

  /** Where a key moves the playhead, or null if it isn't one of ours. */
  #keySeek(key) {
    const now = this.#audio.currentTime || 0;
    switch (key) {
      case "ArrowLeft":
      case "ArrowDown":
        return now - 1;
      case "ArrowRight":
      case "ArrowUp":
        return now + 1;
      case "PageDown":
        return now - 5;
      case "PageUp":
        return now + 5;
      case "Home":
        return 0;
      case "End":
        return this.#duration;
      default:
        return null;
    }
  }

  /** Move the playhead, and everything that follows it, to a second in the clip. */
  #seek(seconds) {
    this.#audio.currentTime = Math.min(this.#duration, Math.max(0, seconds));
    this.#tick();
  }

  connectedCallback() {
    this.#resize.observe(this.#stage);
    if (this.getAttribute("src") !== this.#loading) this.#load();
  }

  disconnectedCallback() {
    this.#resize.disconnect();
    cancelAnimationFrame(this.#frame);
    this.#audio.pause();
    this.#audio.removeAttribute("src");
    URL.revokeObjectURL(this.#url);
    this.#loading = "";
  }

  attributeChangedCallback(name) {
    if (!this.isConnected) return;
    if (name === "src") this.#load();
    else this.#layout();
  }

  get marks() {
    return this.#marks;
  }

  set marks(marks) {
    this.#marks = marks ?? [];
    this.#layout();
  }

  /** Plays the clip, or pauses it if it is playing. Does nothing until the clip has loaded. */
  toggle() {
    if (this.#button.disabled) return;
    if (this.#audio.paused) this.#audio.play().catch((error) => this.#say(`Couldn't play the clip: ${error.message}`));
    else this.#audio.pause();
  }

  get #clipStart() {
    return Number(this.getAttribute("clip-start")) || 0;
  }

  /**
   * Fetches the clip once: the bytes become the <audio> source, as a blob URL,
   * and a copy of them is decoded for the picture. A clip is FLAC, or WAV if it
   * was cut before FLAC, so the server's own content type is what the blob gets.
   */
  async #load() {
    const src = this.getAttribute("src") ?? "";
    this.#loading = src;
    this.#audio.pause();
    this.#image = null;
    this.#duration = 0;
    this.#button.disabled = true;
    this.#layout();
    this.#draw();
    if (!src) return;
    this.#say("Loading the clip…");

    try {
      const res = await fetch(src, { headers: { Accept: "audio/flac, audio/wav" } });
      if (!res.ok) {
        const payload = await res.json().catch(() => null);
        throw new Error(payload?.error ?? `the server answered ${res.status}`);
      }
      const type = res.headers.get("Content-Type") || "audio/flac";
      const bytes = await res.arrayBuffer();
      if (src !== this.#loading) return;

      URL.revokeObjectURL(this.#url);
      this.#url = URL.createObjectURL(new Blob([bytes], { type }));
      this.#audio.src = this.#url;
      this.#button.disabled = false;

      // Decode at the clip's own rate, so a 24 kHz recording isn't stretched
      // over an empty top half. decodeAudioData takes the buffer it is given.
      const context = new OfflineAudioContext({ numberOfChannels: 1, length: 1, sampleRate: clipRate(bytes) ?? 48_000 });
      const audio = await context.decodeAudioData(bytes.slice(0));
      if (src !== this.#loading) return;

      const spectrum = spectrogram(audio.getChannelData(0), audio.sampleRate);
      this.#duration = audio.duration;
      this.#topHz = spectrum.topHz;
      this.#image = this.#paint(spectrum);
      this.#say("");
    } catch (error) {
      if (src !== this.#loading) return;
      this.#say(
        this.#button.disabled
          ? `Couldn't load the clip: ${error.message}`
          : `The clip plays, but its spectrogram couldn't be drawn: ${error.message}`,
      );
    }
    this.#layout();
    this.#draw();
  }

  #say(text) {
    this.#status.textContent = text;
  }

  /** Axis labels, the highlight and the playhead, from the attributes and the clip's length. */
  #layout() {
    const start = this.#clipStart;
    const length = this.#duration;
    const at = (seconds) => `${Math.min(100, Math.max(0, ((seconds - start) / length) * 100))}%`;

    const kHz = this.#topHz / 1000;
    const step = kHz > 6 ? 2 : 1;
    const freqs = [];
    for (let k = 0; k <= kHz + 1e-9; k += step) freqs.push(k);
    this.#freq.innerHTML = this.#image
      ? freqs.map((k) => `<span style="bottom: ${(k / kHz) * 100}%">${k} kHz</span>`).join("")
      : "";
    this.#canvas.setAttribute(
      "aria-label",
      `Spectrogram of the clip, 0 to ${Math.round(kHz)} kHz${length ? `, ${Math.round(length)} seconds` : ""}`,
    );

    this.shadowRoot.querySelector(".plot").style.setProperty("--lanes", Math.max(1, this.#marks.length));
    if (!length) {
      this.#ticks.innerHTML = this.#bands.innerHTML = "";
      this.#playhead.hidden = this.#legend.hidden = true;
      return;
    }
    const every = [1, 2, 5, 10, 15, 30].find((s) => length / s <= 8) ?? 60;
    const ticks = [];
    for (let t = Math.ceil(start / every) * every; t <= start + length + 1e-9; t += every) ticks.push(t);
    this.#ticks.innerHTML = ticks.map((t) => `<span style="left: ${at(t)}">${clock(t)}</span>`).join("");

    const colour = (i) => {
      const n = (i % SPECIES_COLOURS) + 1;
      return `--mark: var(--bs-species-${n}); --mark-ink: var(--bs-species-${n}-ink);`;
    };
    this.#bands.innerHTML = this.#marks
      .flatMap((mark, lane) =>
        mark.spans
          .filter((s) => s.endSec > s.startSec && s.endSec > start && s.startSec < start + length)
          .map(
            (s) =>
              `<div class="band" style="${colour(lane)} --lane: ${lane}; left: ${at(s.startSec)}; width: calc(${at(s.endSec)} - ${at(s.startSec)})"></div>`,
          ),
      )
      .join("");
    this.#legend.hidden = this.#marks.length === 0;
    this.#legend.innerHTML = `${this.#marks
      .map((mark, i) => `<span class="key"><span class="swatch" style="${colour(i)}"></span>${escapeHTML(mark.label)}</span>`)
      .join("")}<span>Click the spectrogram, or focus it and use ← →, to move the playhead</span>`;
    this.#playhead.hidden = false;
    this.#tick();
  }

  /** Scales the painted spectrogram onto the canvas at the screen's resolution. */
  #draw() {
    const width = Math.max(1, Math.round(this.#stage.clientWidth * devicePixelRatio));
    const height = Math.max(1, Math.round(this.#stage.clientHeight * devicePixelRatio));
    this.#canvas.width = width;
    this.#canvas.height = height;
    const ctx = this.#canvas.getContext("2d");
    ctx.clearRect(0, 0, width, height);
    if (!this.#image) return;
    ctx.imageSmoothingEnabled = true;
    ctx.imageSmoothingQuality = "high";
    ctx.drawImage(this.#image, 0, 0, width, height);
  }

  /** Moves the playhead and the clock to where the audio is, every frame while it plays. */
  #tick = () => {
    cancelAnimationFrame(this.#frame);
    const now = this.#audio.currentTime || 0;
    if (this.#duration) this.#playhead.style.left = `${Math.min(100, (now / this.#duration) * 100)}%`;
    this.#time.textContent = this.#duration
      ? `${clock(this.#clipStart + now)} · ${Math.max(0, now).toFixed(1)} of ${this.#duration.toFixed(1)} s`
      : clock(this.#clipStart);
    this.#stage.setAttribute("aria-valuemax", this.#duration.toFixed(1));
    this.#stage.setAttribute("aria-valuenow", Math.max(0, now).toFixed(1));
    this.#stage.setAttribute(
      "aria-valuetext",
      this.#duration ? `${clock(this.#clipStart + now)}, ${now.toFixed(1)} of ${this.#duration.toFixed(1)} seconds` : "No clip",
    );
    if (!this.#audio.paused) this.#frame = requestAnimationFrame(this.#tick);
  };

  /**
   * Paints dB values as ink on paper, in the page's own colours. Contrast comes
   * from the recording: its median is the noise floor, left as paper, and its
   * loudest thousandth is full ink.
   */
  #paint({ columns, bins, db }) {
    const step = Math.max(1, Math.floor(db.length / 20_000));
    const sample = [];
    for (let i = 0; i < db.length; i += step) sample.push(db[i]);
    sample.sort((a, b) => a - b);
    const floor = sample[Math.floor(sample.length * 0.5)];
    const peak = Math.max(sample[Math.floor(sample.length * 0.999)], floor + 20);

    const paper = this.#color("--bs-surface", [255, 255, 255]);
    const ink = this.#color("--bs-text", [0, 0, 0]);
    const image = new ImageData(columns, bins);
    for (let c = 0; c < columns; c++) {
      for (let b = 0; b < bins; b++) {
        const v = Math.min(1, Math.max(0, (db[c * bins + b] - floor) / (peak - floor))) ** 0.8;
        const i = ((bins - 1 - b) * columns + c) * 4;
        image.data[i] = paper[0] + (ink[0] - paper[0]) * v;
        image.data[i + 1] = paper[1] + (ink[1] - paper[1]) * v;
        image.data[i + 2] = paper[2] + (ink[2] - paper[2]) * v;
        image.data[i + 3] = 255;
      }
    }
    const canvas = document.createElement("canvas");
    canvas.width = columns;
    canvas.height = bins;
    canvas.getContext("2d").putImageData(image, 0, 0);
    return canvas;
  }

  /** A design token as [r, g, b]. A canvas normalizes any CSS colour it is given. */
  #color(token, fallback) {
    const probe = document.createElement("canvas").getContext("2d");
    probe.fillStyle = "#000";
    probe.fillStyle = getComputedStyle(this).getPropertyValue(token).trim() || "#000";
    const hex = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(probe.fillStyle);
    if (hex) return hex.slice(1).map((h) => parseInt(h, 16));
    const rgb = probe.fillStyle.match(/[\d.]+/g);
    return rgb ? rgb.slice(0, 3).map(Number) : fallback;
  }
}

/** The rate a clip was cut at, or null if the bytes are neither FLAC nor WAV. */
function clipRate(bytes) {
  return flacRate(bytes) ?? wavRate(bytes);
}

/** The sample rate in a WAV header, or null if the bytes aren't a WAV. */
function wavRate(bytes) {
  if (bytes.byteLength < 28) return null;
  const view = new DataView(bytes);
  const tag = (at) => String.fromCharCode(...new Uint8Array(bytes, at, 4));
  if (tag(0) !== "RIFF" || tag(8) !== "WAVE" || tag(12) !== "fmt ") return null;
  return view.getUint32(24, true) || null;
}

/**
 * The sample rate in a FLAC header, or null if the bytes aren't a FLAC. "fLaC"
 * is followed by a 4-byte block header and then STREAMINFO, whose rate is the
 * 20 bits at byte 18: block and frame sizes take the 10 bytes before it.
 */
function flacRate(bytes) {
  if (bytes.byteLength < 21) return null;
  const head = new Uint8Array(bytes, 0, 21);
  if (head[0] !== 0x66 || head[1] !== 0x4c || head[2] !== 0x61 || head[3] !== 0x43) return null;
  return ((head[18] << 12) | (head[19] << 4) | (head[20] >> 4)) || null;
}

/**
 * Short-time Fourier transform of a mono signal, as dB per column and bin,
 * from 0 Hz up to TOP_HZ (or the Nyquist frequency, if that is lower).
 */
function spectrogram(samples, sampleRate) {
  const size = 2 ** Math.round(Math.log2(sampleRate / BIN_HZ));
  const topHz = Math.min(TOP_HZ, sampleRate / 2);
  const bins = Math.max(1, Math.round((topHz / sampleRate) * size));
  const hop = Math.max(size / 4, Math.ceil((samples.length - size) / MAX_COLUMNS));
  const columns = Math.max(1, Math.floor((samples.length - size) / hop) + 1);

  const hann = new Float64Array(size);
  for (let i = 0; i < size; i++) hann[i] = 0.5 - 0.5 * Math.cos((2 * Math.PI * i) / (size - 1));
  const re = new Float64Array(size);
  const im = new Float64Array(size);
  const db = new Float32Array(columns * bins);
  for (let c = 0; c < columns; c++) {
    const at = c * hop;
    for (let i = 0; i < size; i++) {
      re[i] = (samples[at + i] ?? 0) * hann[i];
      im[i] = 0;
    }
    fft(re, im);
    for (let b = 0; b < bins; b++) db[c * bins + b] = 10 * Math.log10(re[b] * re[b] + im[b] * im[b] + 1e-12);
  }
  return { columns, bins, db, topHz: (bins * sampleRate) / size };
}

const trig = new Map();

/** In-place iterative radix-2 FFT; re and im have a power-of-two length. */
function fft(re, im) {
  const n = re.length;
  if (!trig.has(n)) {
    const cos = new Float64Array(n / 2);
    const sin = new Float64Array(n / 2);
    for (let k = 0; k < n / 2; k++) {
      cos[k] = Math.cos((-2 * Math.PI * k) / n);
      sin[k] = Math.sin((-2 * Math.PI * k) / n);
    }
    trig.set(n, { cos, sin });
  }
  const { cos, sin } = trig.get(n);

  for (let i = 1, j = 0; i < n; i++) {
    let bit = n >> 1;
    for (; j & bit; bit >>= 1) j ^= bit;
    j ^= bit;
    if (i < j) {
      let t = re[i];
      re[i] = re[j];
      re[j] = t;
      t = im[i];
      im[i] = im[j];
      im[j] = t;
    }
  }
  for (let len = 2; len <= n; len <<= 1) {
    const half = len >> 1;
    const stride = n / len;
    for (let start = 0; start < n; start += len) {
      for (let k = 0; k < half; k++) {
        const wr = cos[k * stride];
        const wi = sin[k * stride];
        const j = start + k + half;
        const xr = re[j] * wr - im[j] * wi;
        const xi = re[j] * wi + im[j] * wr;
        re[j] = re[start + k] - xr;
        im[j] = im[start + k] - xi;
        re[start + k] += xr;
        im[start + k] += xi;
      }
    }
  }
}

customElements.define("bs-spectrogram", Spectrogram);
