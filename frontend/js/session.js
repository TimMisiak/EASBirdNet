// Who is signed in. One module holds it so the header, the route guard and the
// admin screens all agree, and so a sign-out updates every one of them.

import * as api from "./api.js";

let current = null;
let loaded = false;
let dev = false;
let providers = [];
const listeners = new Set();

/** The signed-in person, or null. Synchronous: call load() once at startup. */
export const user = () => current;
export const isSignedIn = () => current !== null;
export const isAdmin = () => current?.role === "admin";
/** True when the server runs in development mode (see BIRDSENSE_DB=local). */
export const isDev = () => dev;
/** The identity providers this server can sign people in with, e.g. ["microsoft"]. */
export const identityProviders = () => providers;
/** False until the first load() resolves, so the shell can hold off routing. */
export const isLoaded = () => loaded;

export async function load() {
  try {
    // Read the fields rather than destructuring with defaults: a default only
    // fills in for undefined, and an older server answers with a null list.
    const session = await api.fetchSession();
    current = session.user ?? null;
    dev = session.dev === true;
    providers = session.providers ?? [];
  } catch {
    // A failed session check means anonymous; the public page still works.
    current = null;
    dev = false;
    providers = [];
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
