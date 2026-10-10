/**
 * Admin API client: session token storage + JSON fetch wrapper.
 *
 * Token changes are broadcast to subscribers (auth UI, data refresh) so this
 * module stays free of UI concerns.
 */

const API = "";

// The login session must survive new tabs (and browser restarts, until the
// server-side TTL expires), so it lives in localStorage, which is shared by
// every tab on this origin. sessionStorage is tab-local, which is exactly why
// a new tab used to land on the login page.
const TOKEN_KEY = "adminToken";

function readStoredToken() {
  try {
    // One-time migration: a session stored by an older build keeps working.
    const legacy = sessionStorage.getItem(TOKEN_KEY);
    if (legacy) {
      localStorage.setItem(TOKEN_KEY, legacy);
      sessionStorage.removeItem(TOKEN_KEY);
      return legacy;
    }
    return localStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

let token = readStoredToken();
const tokenListeners = new Set();

/** Subscribe to token changes. The listener receives the new token string. */
export function onTokenChange(listener) {
  tokenListeners.add(listener);
}

export function getToken() {
  return token;
}

export function setToken(value) {
  token = value || "";
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
    sessionStorage.removeItem(TOKEN_KEY);
  } catch {}
  for (const listener of tokenListeners) listener(token);
}

// A login/logout in another tab applies here too, so tabs never disagree
// about the session state. The event only fires in tabs other than the one
// that made the change.
try {
  window.addEventListener("storage", (event) => {
    if (event.key !== TOKEN_KEY) return;
    const next = event.newValue || "";
    if (next === token) return;
    token = next;
    for (const listener of tokenListeners) listener(token);
  });
} catch {}

export async function api(path, options = {}) {
  const res = await fetch(`${API}${path}`, {
    ...options,
    headers: {
      "content-type": "application/json",
      ...(token ? { authorization: `Bearer ${token}` } : {}),
      ...(options.headers || {}),
    },
  });
  if (!res.ok) {
    let message = res.statusText;
    let code = "";
    try {
      const body = await res.json();
      message = body.error?.message || JSON.stringify(body);
      code = body.error?.code || "";
    } catch {}
    // Only a rejected/expired *session* logs the browser out. Handler-level
    // 401s (e.g. wrong current password) must not clear the login token,
    // otherwise the retry goes out unauthenticated ("Admin token required").
    if (res.status === 401 && token && code === "admin_auth_required") {
      setToken("");
    }
    throw new Error(`${res.status}: ${message}`);
  }
  return res.json();
}
