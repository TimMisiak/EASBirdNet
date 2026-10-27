// Who may do what in the browser (js/roles.js), which has to match the
// server's db.User.Has or a tab leads to a 403.

import { test } from "node:test";
import assert from "node:assert/strict";

import { ADMIN, BIRDING_EXPERT, FIELD_VOLUNTEER, REVIEWER, has, labels } from "../../frontend/js/roles.js";

const volunteer = (...roles) => ({ role: "volunteer", roles });

test("an admin may do everything, with or without roles", () => {
  for (const role of [ADMIN, FIELD_VOLUNTEER, REVIEWER, BIRDING_EXPERT]) {
    assert.equal(has({ role: ADMIN, roles: [] }, role), true, role);
  }
});

test("a volunteer may do what their roles say, and a birding expert may review", () => {
  assert.equal(has(volunteer(FIELD_VOLUNTEER), FIELD_VOLUNTEER), true);
  assert.equal(has(volunteer(FIELD_VOLUNTEER), REVIEWER), false);
  assert.equal(has(volunteer(REVIEWER), BIRDING_EXPERT), false);
  assert.equal(has(volunteer(BIRDING_EXPERT), REVIEWER), true);
  assert.equal(has(volunteer(BIRDING_EXPERT), FIELD_VOLUNTEER), false);
  assert.equal(has(volunteer(FIELD_VOLUNTEER, REVIEWER, BIRDING_EXPERT), ADMIN), false);
});

test("no one, or a person from an older server with no roles, may do nothing", () => {
  assert.equal(has(null, FIELD_VOLUNTEER), false);
  assert.equal(has({ role: "volunteer" }, FIELD_VOLUNTEER), false);
});

test("labels name what someone holds, without Reviewer beside Birding expert", () => {
  assert.deepEqual(labels(volunteer(FIELD_VOLUNTEER, REVIEWER)), ["Field volunteer", "Reviewer"]);
  assert.deepEqual(labels(volunteer(REVIEWER, BIRDING_EXPERT)), ["Birding expert"]);
  assert.deepEqual(labels({ role: ADMIN, roles: [FIELD_VOLUNTEER] }), ["Admin", "Field volunteer"]);
  assert.deepEqual(labels({ role: ADMIN, roles: [] }), ["Admin"]);
});
