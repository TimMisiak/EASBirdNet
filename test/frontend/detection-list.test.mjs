// What the Detections tab's export is saved as (js/detection-list.js).

import { test } from "node:test";
import assert from "node:assert/strict";

import { exportName, viewFrom } from "../../frontend/js/detection-list.js";

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
