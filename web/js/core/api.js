/**
 * Admin API client: session token storage + JSON fetch wrapper.
 *
 * Token changes are broadcast to subscribers (auth UI, data refresh) so this
 * module stays free of UI concerns.
 */

const API = "";

let token = sessionStorage.getItem("adminToken") || "";
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
  if (token) sessionStorage.setItem("adminToken", token);
  else sessionStorage.removeItem("adminToken");
  for (const listener of tokenListeners) listener(token);
}

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
