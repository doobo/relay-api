/** Session lifecycle: the login page, logout, and the header account menu. */

import { api, getToken, onTokenChange, setToken } from "./api.js";
import { el } from "./dom.js";
import { clearNotice, notifyError } from "./notice.js";
import { loadRole } from "./perms.js";
import { selectTab } from "./tabs.js";

let wasLoggedIn = false;
let pendingSignOutStatus = null;

/**
 * Message the login page shows after a sign-out the user triggered (a password
 * change, where the server invalidated every session). Rendered - and cleared -
 * the next time the login view appears, so it is never shown twice.
 */
export function setSignOutStatus(message) {
  pendingSignOutStatus = message;
}

/**
 * Switch between the dedicated login page and the app view. The header, tab
 * bar and main content are all tagged .app-view.
 */
function renderAuthUi(loggedIn) {
  el("loginView").classList.toggle("hidden", loggedIn);
  for (const node of document.querySelectorAll(".app-view")) {
    node.classList.toggle("hidden", !loggedIn);
  }

  // Only report a sign-out (logout / expired session), never the first load.
  // The line is the same element core/notice.js renders login failures into,
  // so drop the failure colour before writing a status message.
  const notice = el("loginNotice");
  notice.classList.toggle("hidden", loggedIn || !wasLoggedIn);
  if (!loggedIn && wasLoggedIn) {
    notice.classList.remove("error");
    notice.textContent = pendingSignOutStatus || "Session ended. Sign in to continue.";
    pendingSignOutStatus = null;
  }
  wasLoggedIn = loggedIn;

  if (!loggedIn) closeUserMenu();
  if (loggedIn) {
    // loadRole() also records the role for core/perms.js, so the header name
    // and the read-only decision come from the same /me call.
    loadRole()
      .then((me) => {
        el("authUser").textContent = me.user ? me.user.username : "admin token";
      })
      .catch(() => {});
  }
}

/** Username dropdown holding the account actions. */
function openUserMenu(open) {
  el("userMenuPanel").classList.toggle("hidden", !open);
  el("userMenuToggle").setAttribute("aria-expanded", String(open));
}

function closeUserMenu() {
  openUserMenu(false);
}

function initUserMenu() {
  const menu = el("userMenu");
  const panel = el("userMenuPanel");

  el("userMenuToggle").onclick = () => openUserMenu(panel.classList.contains("hidden"));
  // Both entries leave the menu (one jumps to Settings, one logs out), so the
  // menu closes either way.
  el("accountMenuBtn").addEventListener("click", () => {
    closeUserMenu();
    selectTab("settings");
  });
  el("logoutBtn").addEventListener("click", closeUserMenu);

  document.addEventListener("click", (e) => {
    if (!menu.contains(e.target)) closeUserMenu();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeUserMenu();
  });
}

/** Base64-encode bytes (btoa only takes a binary string). */
function toBase64(buffer) {
  const bytes = new Uint8Array(buffer);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/** Base64-decode a binary string into bytes. */
function fromBase64(value) {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

/**
 * Preferred scheme: fresh AES-GCM key encrypts {username, password, nonce},
 * wrapped with the server's RSA-OAEP public key. Needs crypto.subtle, which
 * browsers only expose in a secure context (https:// or http://localhost).
 */
async function encryptWithWebCrypto(challenge, username, password) {
  const publicKey = await crypto.subtle.importKey(
    "jwk",
    challenge.publicKey,
    { name: "RSA-OAEP", hash: "SHA-256" },
    false,
    ["encrypt"],
  );

  const aesKey = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, true, [
    "encrypt",
  ]);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const payload = new TextEncoder().encode(
    JSON.stringify({ username, password, nonce: challenge.nonce }),
  );
  const data = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, aesKey, payload);
  const rawKey = await crypto.subtle.exportKey("raw", aesKey);
  const key = await crypto.subtle.encrypt({ name: "RSA-OAEP" }, publicKey, rawKey);

  return { key: toBase64(key), iv: toBase64(iv), data: toBase64(data) };
}

/**
 * Fallback for insecure contexts (plain HTTP on a LAN address), where
 * crypto.subtle is unavailable: TweetNaCl's anonymous box (X25519 +
 * XSalsa20-Poly1305), loaded on demand. Only crypto.getRandomValues is needed,
 * which browsers expose even without a secure context.
 */
async function encryptWithTweetNacl(challenge, username, password) {
  if (!globalThis.crypto?.getRandomValues || !challenge.box?.publicKey) {
    throw new Error(
      "This browser cannot encrypt the login; open the admin UI in an up-to-date browser.",
    );
  }
  const { nacl } = await import("./nacl.js");

  const keyPair = nacl.box.keyPair();
  const nonce = nacl.randomBytes(nacl.box.nonceLength);
  const payload = new TextEncoder().encode(
    JSON.stringify({ username, password, nonce: challenge.nonce }),
  );
  const data = nacl.box(payload, nonce, fromBase64(challenge.box.publicKey), keyPair.secretKey);

  return {
    publicKey: toBase64(keyPair.publicKey),
    nonce: toBase64(nonce),
    data: toBase64(data),
  };
}

/**
 * Fetches a one-time challenge and encrypts the credentials with whichever
 * scheme this browser supports. The password is never sent in the clear.
 */
async function buildLoginBody(username, password) {
  const challenge = await api("/admin/auth/challenge");
  if (globalThis.crypto?.subtle && challenge.publicKey) {
    return { encrypted: await encryptWithWebCrypto(challenge, username, password) };
  }
  return { box: await encryptWithTweetNacl(challenge, username, password) };
}

export function initAuth() {
  const loginForm = el("loginForm");

  // A real form, so Enter submits it natively on every platform.
  loginForm.onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(loginForm);
    const username = el("loginUsername").value.trim();
    const password = el("loginPassword").value;
    if (!username || !password) return notifyError("Enter username and password.", { form: loginForm });
    try {
      // The password never leaves the browser in the clear.
      const body = await buildLoginBody(username, password);
      const res = await api("/admin/auth/login", {
        method: "POST",
        body: JSON.stringify(body),
      });
      loginForm.reset();
      setToken(res.token);
    } catch (e) {
      notifyError(`Login failed: ${e.message}`, { form: loginForm });
      el("loginPassword").focus();
    }
  };

  // Typing again clears the previous failure.
  for (const id of ["loginUsername", "loginPassword"]) {
    el(id).addEventListener("input", () => clearNotice(loginForm));
  }

  el("logoutBtn").onclick = async () => {
    try {
      await api("/admin/auth/logout", { method: "POST" });
    } catch {}
    setToken("");
  };

  initUserMenu();

  onTokenChange(renderAuthUi);
  renderAuthUi(Boolean(getToken()));
}
