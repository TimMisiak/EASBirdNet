// How the Detections tab reads its view off a query string, and what its export is saved as (js/detection-list.js).

import { test } from "node:test";
import assert from "node:assert/strict";

import { exportName, keyOf, paramsFor, viewFrom } from "../../frontend/js/detection-list.js";

const dates = { from: "2026-10-10", to: "2026-10-17" };

test("an export is named for the dates its view covers", () => {
  assert.equal(exportName(viewFrom(new URLSearchParams(), dates)), "detections-2026-10-10-to-2026-10-17.json");
  assert.equal(
    exportName(viewFrom(new URLSearchParams("from=2026-09-01&to=2026-09-30&species=Strix+varia&page=3"), dates)),
    "detections-2026-09-01-to-2026-09-30.json",
  );
});

test("an export with a date cleared says what is left", () => {
  // A cleared field is an empty string in the view, which the server bounds with its own window.
  assert.equal(exportName({ ...viewFrom(new URLSearchParams(), dates), from: "" }), "detections-until-2026-10-17.json");
  assert.equal(exportName({ ...viewFrom(new URLSearchParams(), dates), to: "" }), "detections-2026-10-10.json");
  assert.equal(exportName({ ...viewFrom(new URLSearchParams(), dates), from: "", to: "" }), "detections.json");
});

test("one card's view is the whole card unless it names dates", () => {
  const card = viewFrom(new URLSearchParams("card=OWL-20260914-SR03"), dates);
  assert.equal(card.card, "OWL-20260914-SR03");
  assert.equal(card.from, "");
  assert.equal(card.to, "");
  assert.equal(paramsFor(card).toString(), "card=OWL-20260914-SR03");
  assert.equal(exportName(card), "detections-OWL-20260914-SR03.json");

  const narrowed = viewFrom(new URLSearchParams("card=OWL-20260914-SR03&from=2026-09-10"), dates);
  assert.equal(narrowed.from, "2026-09-10");
  assert.equal(narrowed.to, "");
});

test("a view is BirdNET's unless it asks for Perch's", () => {
  assert.equal(viewFrom(new URLSearchParams(), dates).model, "");
  assert.equal(viewFrom(new URLSearchParams("model=birdnet"), dates).model, "");
  assert.equal(viewFrom(new URLSearchParams("model=owlnet"), dates).model, "");
  const perch = viewFrom(new URLSearchParams("card=OWL-20260914-SR03&model=perch"), dates);
  assert.equal(perch.model, "perch");
  assert.equal(paramsFor(perch).toString(), "card=OWL-20260914-SR03&model=perch");
  assert.equal(exportName(perch), "perch-detections-OWL-20260914-SR03.json");
  // Each model's list is held apart, so stepping never crosses from one to the other.
  assert.notEqual(keyOf(perch), keyOf({ ...perch, model: "" }));
});
