/**
 * BirdNET's species list (/data/birdnet-species.json, from analyzer/species.py)
 * as something to look a name up in: either name, typed loosely -- any case,
 * a hyphen as a space, a curly apostrophe for a straight one -- finds the
 * species as BirdNET spells it, which is the spelling a rule has to match.
 */

export const SPECIES_LIST = "/data/birdnet-species.json";

/** A name as it is compared: lower case, hyphens and runs of space as one space. */
export const fold = (name) =>
  String(name ?? "")
    .replace(/[‘’]/g, "'")
    .replace(/[\s-]+/g, " ")
    .trim()
    .toLowerCase();

/**
 * An index over [scientificName, commonName] pairs.
 *
 * find(text, kind) is the species whose name `text` is, or null; `kind`
 * ("scientific" or "common") says which name to look at, and with none either
 * will do, the scientific first.
 *
 * suggest(text, kind, limit) is the species whose `kind` name begins with
 * `text`, then those with a word that does, then those that merely contain
 * it -- each group in order of that name.
 */
export function speciesIndex(pairs) {
  const species = pairs
    .filter((p) => Array.isArray(p) && p[0])
    .map(([scientificName, commonName]) => ({
      scientificName,
      commonName: commonName || scientificName,
      scientific: fold(scientificName),
      common: fold(commonName || scientificName),
    }));
  const by = { scientific: new Map(), common: new Map() };
  for (const s of species) {
    by.scientific.set(s.scientific, s);
    by.common.set(s.common, s);
  }
  const plain = (s) => (s ? { scientificName: s.scientificName, commonName: s.commonName } : null);
  const sorted = {
    scientific: [...species].sort((a, b) => a.scientific.localeCompare(b.scientific)),
    common: [...species].sort((a, b) => a.common.localeCompare(b.common)),
  };

  return {
    size: species.length,
    find(text, kind) {
      const key = fold(text);
      if (!key) return null;
      if (kind) return plain(by[kind].get(key));
      return plain(by.scientific.get(key) ?? by.common.get(key));
    },
    suggest(text, kind, limit = 12) {
      const needle = fold(text);
      if (needle.length < 2) return [];
      const ranked = [[], [], []];
      for (const s of sorted[kind]) {
        const name = s[kind];
        const at = name.indexOf(needle);
        if (at === 0) ranked[0].push(s);
        else if (at > 0 && name.includes(` ${needle}`)) ranked[1].push(s);
        else if (at > 0) ranked[2].push(s);
      }
      return ranked.flat().slice(0, limit).map(plain);
    },
  };
}
