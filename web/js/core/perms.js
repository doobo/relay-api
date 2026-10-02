/**
 * Account role state for the admin UI.
 *
 * Roles come from GET /admin/auth/me: `admin` may change things, `user` is
 * read-only. Two consumers:
 *
 * - CSS: the role is mirrored onto <html data-role>, so `html[data-role="user"]
 *   .admin-only { display: none }` hides every mutating control at once,
 *   including buttons a tab builds later (`smallButton`/`delButton` tag their
 *   output, see core/dom.js).
 * - Tabs: `isAdmin()` for the few places that must skip work rather than hide
 *   something (e.g. not fetching the account list).
 *
 * The server is the actual authority - the admin APIs reject non-GET requests
 * from a read-only account - so this only keeps the UI honest.
 */

import { api, onTokenChange } from "./api.js";

let admin = false;
let inflight = null;

export function isAdmin() {
  return admin;
}

/** Remember the role and reflect it on <html>. */
export function setAdmin(value) {
  admin = Boolean(value);
  document.documentElement.dataset.role = admin ? "admin" : "user";
}

/**
 * Resolve the current role once per session token. Callers await this before
 * rendering a tab, so nothing privileged is built for a read-only account.
 */
export function loadRole() {
  if (!inflight) {
    inflight = api("/admin/auth/me")
      .then((me) => {
        // The static ADMIN_TOKEN has no user row but keeps full rights.
        setAdmin(me.via === "token" || me.user?.role === "admin");
        return me;
      })
      .catch((error) => {
        inflight = null; // a failed probe must not be cached
        throw error;
      });
  }
  return inflight;
}

// Start out read-only: until /me answers, showing nothing is the safe default.
setAdmin(false);

// A different token means a different account, so drop the cached answer (and
// hide privileged controls again) rather than trusting the previous role.
onTokenChange((token) => {
  inflight = null;
  if (!token) setAdmin(false);
});
