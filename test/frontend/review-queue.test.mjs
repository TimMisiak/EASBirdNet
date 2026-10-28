// The Review tab's queue: its order, stepping, what waits for the reviewer,
// and the species a confirm can name.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  knownSpecies,
  nextForMe,
  parseSpecies,
  progress,
  queue,
  standing,
  step,
  verdictLabel,
} from "../../frontend/js/review-queue.js";

const bout = (id, kind, scientificName, commonName, at, extra = {}) => ({
  id,
  kind,
  scientificName,
  commonName,
  firstHeardAt: `2026-10-03T0${at}:00:00Z`,
  status: "open",
  forMe: true,
  ...extra,
});

const bouts = [
  bout("b1", "review", "Bubo virginianus", "Great Horned Owl", 5),
  bout("b2", "spotcheck", "Corvus brachyrhynchos", "American Crow", 1),
  bout("b3", "review", "Strix varia", "Barred Owl", 4),
  bout("b4", "review", "Bubo virginianus", "Great Horned Owl", 2, { forMe: false, mine: { verdict: "confirm" } }),
  bout("b5", "review", "Strix varia", "Barred Owl", 1, { status: "confirmed", forMe: false }),
  bout("b6", "spotcheck", "Branta canadensis", "Canada Goose", 3, { status: "disputed", forMe: false }),
];

test("review bouts come first, by species, each species' in the order heard", () => {
  const { groups, order } = queue(bouts);
  assert.deepEqual(groups.map((g) => g.kind), ["review", "spotcheck"]);
  assert.deepEqual(groups[0].species.map((s) => s.commonName), ["Barred Owl", "Great Horned Owl"]);
  assert.deepEqual(order, ["b5", "b3", "b4", "b1", "b2", "b6"]);
  assert.deepEqual(queue([]).groups, []);
});

test("stepping goes along the queue and stops at its ends", () => {
  const { order } = queue(bouts);
  assert.equal(step(order, "b3", 1), "b4");
  assert.equal(step(order, "b3", -1), "b5");
  assert.equal(step(order, "b6", 1), null);
  assert.equal(step(order, "b5", -1), null);
  assert.equal(step(order, "gone", 1), "b5");
});

test("the next bout for the reviewer skips what they've heard, and goes round", () => {
  const { order } = queue(bouts);
  const byId = new Map(bouts.map((b) => [b.id, b]));
  assert.equal(nextForMe(order, byId, null), "b3");
  assert.equal(nextForMe(order, byId, "b3"), "b1");
  assert.equal(nextForMe(order, byId, "b2"), "b3");
  const done = new Map(bouts.map((b) => [b.id, { ...b, forMe: false }]));
  assert.equal(nextForMe(order, done, "b3"), null);
});

test("progress and where each bout stands", () => {
  assert.deepEqual(progress(bouts), { total: 6, settled: 1, disputed: 1, left: 3, given: 1 });
  assert.equal(standing(bouts[0]).mark, "waiting");
  assert.equal(standing(bouts[3]).mark, "given");
  assert.equal(standing(bouts[4]).mark, "confirmed");
  assert.equal(standing(bouts[5]).mark, "disputed");
  const owl = bouts[0];
  assert.equal(verdictLabel({ verdict: "confirm" }, owl), "Confirmed");
  assert.equal(verdictLabel({ verdict: "confirm", scientificName: "Strix varia", commonName: "Barred Owl" }, owl), "Confirmed as Barred Owl");
  assert.equal(verdictLabel({ verdict: "unsure" }, owl), "Unsure");
});

test("another species is one known, by any of its names, or written out", () => {
  const known = knownSpecies(
    [{ scientificName: "Strix varia", commonName: "Barred Owl" }],
    [{ scientificName: "Strix varia", commonName: "Barred Owl" }, { scientificName: "Bubo virginianus", commonName: "Great Horned Owl" }],
  );
  assert.deepEqual(known.map((s) => s.commonName), ["Barred Owl", "Great Horned Owl"]);
  const barred = { scientificName: "Strix varia", commonName: "Barred Owl" };
  assert.deepEqual(parseSpecies("Barred Owl (Strix varia)", known), barred);
  assert.deepEqual(parseSpecies("  barred   owl ", known), barred);
  assert.deepEqual(parseSpecies("strix varia", known), barred);
  assert.deepEqual(parseSpecies("Spotted Owl (Strix occidentalis)", known), {
    scientificName: "Strix occidentalis",
    commonName: "Spotted Owl",
  });
  assert.equal(parseSpecies("Spotted Owl", known), null);
  assert.equal(parseSpecies("Spotted Owl (spotted owl)", known), null);
  assert.equal(parseSpecies("", known), null);
});
