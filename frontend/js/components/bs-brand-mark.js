import { BaseElement } from "./base-element.js";

/**
 * <bs-brand-mark> is the Eastside Audubon logo with the Birdsense wordmark set
 * beside it, after a thin rule -- the lockup a program page uses under the
 * society's own mark. It appears on the public masthead, the sign-in card and
 * the app header, all of them white, so the variants live here rather than
 * being rebuilt three times.
 *
 * The logo is the society's official file (images/eas-logo.png, from
 * eastsideaudubon.org); swap the file rather than redrawing it.
 *
 * Attributes:
 *   variant  "full" (logo, rule, wordmark and a strapline) | "compact" (logo,
 *            rule, wordmark) | "inline" (the same at text size, for the
 *            sign-in card)
 *   size     logo height in px, default 56
 */
class BrandMark extends BaseElement {
  static observedAttributes = ["variant", "size"];

  render() {
    const variant = this.getAttribute("variant") ?? "full";
    const size = Number(this.getAttribute("size")) || 56;

    this.shadowRoot.innerHTML = `
      <style>
        :host {
          display: inline-flex;
          align-items: center;
          gap: ${Math.round(size * 0.22)}px;
          color: var(--bs-navy);
        }
        img { display: block; height: ${size}px; width: auto; }
        .rule {
          align-self: stretch;
          width: 1px;
          margin: ${Math.round(size * 0.12)}px 0;
          background: var(--bs-border-strong);
        }
        .words { line-height: 1.15; text-align: left; }
        .wordmark {
          display: block;
          font-family: var(--bs-font-display);
          font-weight: 700;
          font-size: ${Math.max(15, Math.round(size * 0.36))}px;
          letter-spacing: 0.01em;
        }
        .strap {
          display: block;
          margin-top: 0.125rem;
          font-size: 0.6875rem;
          font-weight: 700;
          letter-spacing: 0.12em;
          text-transform: uppercase;
          color: var(--bs-text-muted);
        }
      </style>
      <img src="/images/eas-logo.png" alt="Eastside Audubon" width="${Math.round(size * 1.53)}" height="${size}" />
      <span class="rule" aria-hidden="true"></span>
      <span class="words">
        <span class="wordmark">Birdsense</span>
        ${variant === "full" ? `<span class="strap">Acoustic monitoring</span>` : ""}
      </span>
    `;
  }
}

customElements.define("bs-brand-mark", BrandMark);
