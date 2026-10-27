// What someone on the roster does, and so which parts of the app they reach.
// The server is the last word (db.User.Has in Go); this is the same rule, so
// the tabs and the route guard show what the API will answer.
//
// A person has a `role`, the access level -- "admin" or "volunteer" -- and
// `roles`, the volunteer roles they hold, any number of them. An admin may do
// everything whatever their roles say, and keeps the roles for if they stop
// being one. A birding expert is a reviewer too.

export const ADMIN = "admin";
export const FIELD_VOLUNTEER = "field_volunteer";
export const REVIEWER = "reviewer";
export const BIRDING_EXPERT = "birding_expert";

/** The volunteer roles, in the order the server stores them, with what each one is for. */
export const VOLUNTEER_ROLES = [
  { id: FIELD_VOLUNTEER, label: "Field volunteer", what: "Uploads SD cards from recorders" },
  { id: REVIEWER, label: "Reviewer", what: "Hears and reviews detections" },
  { id: BIRDING_EXPERT, label: "Birding expert", what: "A reviewer, with the expert review tools" },
];

/** Whether person may do what role allows. ADMIN may be asked for like any other role. */
export function has(person, role) {
  if (!person) return false;
  if (person.role === ADMIN) return true;
  if (role === ADMIN) return false;
  const held = person.roles ?? [];
  return held.includes(role) || (role === REVIEWER && held.includes(BIRDING_EXPERT));
}

/**
 * The names of what person holds, for a label: "Admin" first if they are one,
 * then each volunteer role -- leaving out Reviewer beside Birding expert, which
 * already says it.
 */
export function labels(person) {
  const held = person?.roles ?? [];
  return [
    ...(person?.role === ADMIN ? ["Admin"] : []),
    ...VOLUNTEER_ROLES.filter(
      ({ id }) => held.includes(id) && !(id === REVIEWER && held.includes(BIRDING_EXPERT)),
    ).map(({ label }) => label),
  ];
}
