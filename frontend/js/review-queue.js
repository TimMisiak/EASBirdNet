// A card's bouts as the Review tab works through them (REVIEW.md, Screens):
// the queue's order, where a bout stands for the reviewer, and the species a
// confirm may name instead. No DOM here, so it is tested on its own
// (test/frontend/review-queue.test.mjs).

/** A bout's kinds and statuses, as the API spells them. */
export const REVIEW = "review";
export const SPOT_CHECK = "spotcheck";
export const OPEN = "open";
export const DISPUTED = "disputed";

/** How a verdict reads: "confirm" is "Confirmed", and so on. */
export const VERDICTS = { confirm: "Confirmed", reject: "Rejected", unsure: "Unsure" };

/**
 * The queue down the side of a card's review: bouts routed to review first,
 * then spot-checks of what the rules accepted, each grouped by species --
 * species in the order of their common names, so a reviewer hears one bird's
 * bouts one after another -- and each species' bouts in the order heard.
 *
 * @returns {{groups: {kind, species: {scientificName, commonName, bouts}[]}[], order: string[]}}
 *   order is every bout's id, in the queue's order: what stepping follows.
 */
export function queue(bouts) {
  const groups = [REVIEW, SPOT_CHECK].map((kind) => {
    const bySpecies = new Map();
    for (const b of bouts) {
      if ((b.kind === SPOT_CHECK) !== (kind === SPOT_CHECK)) continue;
      if (!bySpecies.has(b.scientificName)) {
        bySpecies.set(b.scientificName, { scientificName: b.scientificName, commonName: b.commonName, bouts: [] });
      }
      bySpecies.get(b.scientificName).bouts.push(b);
    }
    const species = [...bySpecies.values()].sort(
      (a, b) => a.commonName.localeCompare(b.commonName) || a.scientificName.localeCompare(b.scientificName),
    );
    for (const s of species) {
      s.bouts.sort((a, b) => Date.parse(a.firstHeardAt) - Date.parse(b.firstHeardAt) || a.id.localeCompare(b.id));
    }
    return { kind, species };
  }).filter((g) => g.species.length > 0);
  const order = groups.flatMap((g) => g.species.flatMap((s) => s.bouts.map((b) => b.id)));
  return { groups, order };
}

/** The bout before or after id in order (delta -1 or 1), or null at either end. */
export function step(order, id, delta) {
  const at = order.indexOf(id);
  if (at < 0) return order[0] ?? null;
  return order[at + delta] ?? null;
}

/**
 * The next bout after id, in order, that waits for the reviewer (forMe),
 * going round to the start; null when nothing else does. With no id, the
 * first that waits.
 */
export function nextForMe(order, byId, id) {
  const at = id ? order.indexOf(id) : -1;
  for (let i = 1; i <= order.length; i++) {
    const candidate = order[(at + i + order.length) % order.length];
    if (candidate !== id && byId.get(candidate)?.forMe) return candidate;
  }
  return null;
}

/**
 * Where a card's review stands: {total, settled, disputed, left, given},
 * left being the bouts that wait for the reviewer and given those they have
 * given a verdict on.
 */
export function progress(bouts) {
  const out = { total: bouts.length, settled: 0, disputed: 0, left: 0, given: 0 };
  for (const b of bouts) {
    if (b.status === DISPUTED) out.disputed++;
    else if (b.status !== OPEN) out.settled++;
    if (b.forMe) out.left++;
    if (b.mine) out.given++;
  }
  return out;
}

/**
 * Where a bout stands for the reviewer: {mark, label}, mark being one of
 * "waiting" (for them), "given" (theirs is in, others' aren't), "confirmed",
 * "rejected" or "disputed".
 */
export function standing(bout) {
  if (bout.status === DISPUTED) return { mark: "disputed", label: "Disputed: waits for a birding expert" };
  if (bout.status !== OPEN) {
    return { mark: bout.status, label: bout.status === "confirmed" ? "Confirmed" : "Rejected" };
  }
  if (bout.mine) return { mark: "given", label: `Yours: ${verdictLabel(bout.mine, bout)}; waiting for others` };
  return { mark: "waiting", label: "Waiting for you" };
}

/** "Confirmed", "Confirmed as Barred Owl", "Rejected" or "Unsure". */
export function verdictLabel(verdict, bout) {
  const name = verdict.commonName || verdict.scientificName;
  if (verdict.verdict === "confirm" && name && verdict.scientificName !== bout?.scientificName) return `Confirmed as ${name}`;
  return VERDICTS[verdict.verdict] ?? verdict.verdict;
}

/** How a species is offered when a confirm names another: "Barred Owl (Strix varia)". */
export const speciesOption = ({ commonName, scientificName }) =>
  commonName && commonName !== scientificName ? `${commonName} (${scientificName})` : scientificName;

/**
 * The species a reviewer typed into "another species": one of known
 * ({scientificName, commonName}) by its option, common or scientific name,
 * ignoring case; or a new one written as "Common name (Scientific name)".
 * Null when it is neither.
 */
export function parseSpecies(text, known) {
  const typed = text.trim().replace(/\s+/g, " ");
  if (!typed) return null;
  const lower = typed.toLowerCase();
  const match = known.find(
    (s) =>
      speciesOption(s).toLowerCase() === lower ||
      s.scientificName.toLowerCase() === lower ||
      (s.commonName ?? "").toLowerCase() === lower,
  );
  if (match) return { scientificName: match.scientificName, commonName: match.commonName || match.scientificName };
  const written = /^(.+?)\s*\(([^()]+)\)$/.exec(typed);
  if (!written) return null;
  const scientificName = written[2].trim();
  // A scientific name is two words at least, the genus capitalized.
  if (!/^[A-Z][a-z-]+ [a-z-]+( [a-z-]+)?$/.test(scientificName)) return null;
  return { scientificName, commonName: written[1].trim() };
}

/**
 * Every species a confirm can name, without repeats: the rules' rows and the
 * card's own bouts, in the order of their common names.
 */
export function knownSpecies(...lists) {
  const seen = new Map();
  for (const s of lists.flat()) {
    if (s?.scientificName && !seen.has(s.scientificName)) {
      seen.set(s.scientificName, { scientificName: s.scientificName, commonName: s.commonName || s.scientificName });
    }
  }
  return [...seen.values()].sort((a, b) => a.commonName.localeCompare(b.commonName));
}
