import { BaseElement } from "./base-element.js";

/**
 * <bs-brand-mark> is the "EA" roundel and the Birdsense wordmark. It appears on
 * the public masthead, the sign-in card and the app header, on both the forest
 * green and the paper background, so the variants live here rather than being
 * rebuilt three times.
 *
 * Attributes:
 *   variant  "full" (org name over the wordmark) | "compact" (wordmark only) |
 *            "inline" (one mono line, for the sign-in card)
 *   on       "dark" (default, sits on forest green) | "light"
 *   size     roundel diameter in px, default 34
 */
class BrandMark extends BaseElement {
  static observedAttributes = ["variant", "on", "size"];

  render() {
    const variant = this.getAttribute("variant") ?? "full";
    const onDark = this.getAttribute("on") !== "light";
    const size = Number(this.getAttribute("size")) || 34;

    this.shadowRoot.innerHTML = `
      <style>
        :host { display: inline-flex; align-items: center; gap: ${size < 30 ? "0.5625rem" : "0.75rem"}; }
        .roundel {
          width: ${size}px;
          height: ${size}px;
          flex: none;
          border-radius: 50%;
          border: 2px solid var(--bs-amber);
          display: flex;
          align-items: center;
          justify-content: center;
          font-family: var(--bs-font-mono);
          font-size: ${Math.round(size * 0.38)}px;
          color: ${onDark ? "var(--bs-amber)" : "var(--bs-amber-ink)"};
        }
        .words { line-height: 1.2; }
        .org, .wordmark { display: block; }
        .org {
          font-family: var(--bs-font-display);
          font-size: 1.1875rem;
          letter-spacing: 0.01em;
          color: ${onDark ? "var(--bs-on-forest)" : "var(--bs-text)"};
        }
        :host([variant="inline"]) .wordmark { display: inline; }
        .wordmark {
          font-family: var(--bs-font-mono);
          font-size: 0.65625rem;
          letter-spacing: 0.22em;
          text-transform: uppercase;
          color: ${onDark ? "var(--bs-amber)" : "var(--bs-text-muted)"};
        }
      </style>
      <span class="roundel" aria-hidden="true">EA</span>
      ${
        variant === "inline"
          ? `<span class="wordmark">Eastside Audubon · Birdsense</span>`
          : `<span class="words">
               ${variant === "full" ? `<span class="org">Eastside Audubon</span>` : ""}
               <span class="wordmark">Birdsense</span>
             </span>`
      }
    `;
  }
}

customElements.define("bs-brand-mark", BrandMark);
