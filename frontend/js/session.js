// Who is signed in. One module holds it so the header, the route guard and the
// admin screens all agree, and so a sign-out updates every one of them.

import * as api from "./api.js";

let current = null;
let loaded = false;
const listeners = new Set();

/** The signed-in person, or null. Synchronous: call load() once at startup. */
export const user = () => current;
export const isSignedIn = () => current !== null;
export const isAdmin = () => current?.role === "admin";
/** False until the first load() resolves, so the shell can hold off routing. */
export const isLoaded = () => loaded;

export async function load() {
  try {
    ({ user: current } = await api.fetchSession());
  } catch {
    // A failed session check means anonymous; the public page still works.
    current = null;
  }
  loaded = true;
  announce();
  return current;
}

export async function signIn(body) {
  ({ user: current } = await api.createSession(body));
  announce();
  return current;
}

export async function signOut() {
  await api.deleteSession().catch(() => {});
  current = null;
  announce();
}

export function onChange(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function announce() {
  for (const listener of listeners) listener(current);
}
