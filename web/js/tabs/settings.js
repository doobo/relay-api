/**
 * Settings tab: admin users, enable/disable, password resets.
 *
 * Every message on this tab is inline: row-action failures render in the Admin
 * Users panel, form and dialog failures in the form that caused them, and the
 * "sign in again" status on the login page. No floating toasts and no native
 * confirm()/alert() popups - including the delete confirmation, which is a
 * proper dialog.
 *
 * A password reset left blank generates a random password and shows it exactly
 * once, in place of the form, so reopening the dialog cannot surface it again.
 */

import { api, setToken } from "../core/api.js";
import { setSignOutStatus } from "../core/auth.js";
import { askConfirm, badge, bindCollapsible, closeDialog, delButton, el, fillTable, fillTableMessage, openDialog, smallButton, wrapButtons } from "../core/dom.js";
import { fmtTime } from "../core/format.js";
import { clearNotice, notifyError } from "../core/notice.js";
import { isAdmin } from "../core/perms.js";

let currentUsername = null;

/**
 * Characters for a generated password. Ambiguity-prone glyphs (0/O, 1/l/I) are
 * left out so the value survives being read out loud or copied by hand.
 */
const PASSWORD_ALPHABET = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%^&*-_=+";

/** Random password for a reset that was submitted blank. */
function randomPassword(length = 16) {
  const bytes = crypto.getRandomValues(new Uint8Array(length));
  return Array.from(bytes, (b) => PASSWORD_ALPHABET[b % PASSWORD_ALPHABET.length]).join("");
}

/** Current account card: identity, auth method and password change. */
function renderAccount(me) {
  const username = me.user?.username ?? null;
  el("accountUsername").textContent = username ?? "admin token";
  el("accountAuthMethod").textContent =
    me.via === "session" ? "admin login (session)" : "static ADMIN_TOKEN";
  el("accountLastLogin").textContent = me.user?.last_login_at
    ? fmtTime(me.user.last_login_at)
    : "never";
  // The static token is machine access and keeps full rights; a session is
  // whatever role the account has.
  el("accountRole").textContent =
    me.via === "token" ? "admin (static token)"
    : me.user?.role === "admin" ? "admin (full access)"
    : "read-only (view only)";

  // PUT /admin/auth/password only accepts a login session, so hide the form
  // (and explain why) when the caller is authenticated with the static token.
  const sessionAuth = me.via === "session";
  el("changePwForm").classList.toggle("hidden", !sessionAuth);
  el("changePwHint").textContent = sessionAuth
    ? "Changing the password signs out every session, including this one."
    : "Password changes are unavailable with the static ADMIN_TOKEN - log in with a username and password instead.";
}

/** Inline row-action feedback in the Admin Users panel (never a toast). */
function showUsersNotice(message) {
  const slot = el("adminUsersNotice");
  slot.textContent = message;
  slot.classList.remove("hidden");
}

function clearUsersNotices() {
  el("adminUsersNotice").classList.add("hidden");
}

export async function loadSettings() {
  try {
    const me = await api("/admin/auth/me");
    currentUsername = me.user?.username ?? null;
    renderAccount(me);
    // The account list is a management surface: the server rejects it for a
    // read-only account, and its panel is hidden by CSS anyway.
    if (!isAdmin()) return;
    const data = await api("/admin/auth/users");
    fillTable(
      "adminUsersTable",
      data.data.map((u) => {
        const isSelf = Boolean(currentUsername) && u.username === currentUsername;
        return [
          u.id,
          u.username + (isSelf ? " (you)" : ""),
          roleBadge(u.role),
          badge(u.enabled ? "ON" : "OFF", u.enabled ? "on" : "off"),
          u.last_login_at ? fmtTime(u.last_login_at) : "never",
          fmtTime(u.created_at),
          userActions(u, isSelf),
        ];
      }),
    );
  } catch (e) {
    fillTableMessage("adminUsersTable", `Failed to load: ${e.message}`, { error: true });
  }
}

/** Role cell: an admin may change things, a read-only account may not. */
function roleBadge(role) {
  return badge(role === "admin" ? "admin" : "read-only", role === "admin" ? "on" : "off");
}

/**
 * Confirm in the shared dialog (never a native confirm() popup). The server
 * rejects deleting the last enabled admin, and that message lands inline in the
 * dialog so it can be answered from there.
 */
function askDeleteUser(user) {
  askConfirm({
    title: `Delete admin user ${user.username}?`,
    message: "The account and its sessions are removed immediately. This cannot be undone.",
    action: async () => {
      await api(`/admin/auth/users/${user.id}`, { method: "DELETE" });
      loadSettings();
    },
  });
}

function userActions(u, isSelf) {
  const actions = [
    smallButton("Reset password", () => openResetDialog(u)),
    smallButton(u.enabled ? "Disable" : "Enable", async () => {
      try {
        await api(`/admin/auth/users/${u.id}`, {
          method: "PUT",
          body: JSON.stringify({ enabled: !u.enabled }),
        });
        loadSettings();
      } catch (e) {
        showUsersNotice(e.message);
      }
    }),
  ];
  if (!isSelf) {
    // One field, so a button beats a dialog. The server refuses to demote the
    // last admin, and that message shows in the panel notice above.
    actions.push(
      smallButton(u.role === "admin" ? "Make read-only" : "Make admin", async () => {
        try {
          await api(`/admin/auth/users/${u.id}`, {
            method: "PUT",
            body: JSON.stringify({ role: u.role === "admin" ? "user" : "admin" }),
          });
          loadSettings();
        } catch (e) {
          showUsersNotice(e.message);
        }
      }),
    );
    actions.push(delButton(() => askDeleteUser(u)));
  }
  return wrapButtons(...actions);
}

/** Open the reset dialog on its form, never on a password shown earlier. */
function openResetDialog(user) {
  const form = el("resetPwForm");
  form.reset();
  form.classList.remove("hidden");
  el("resetPwResult").classList.add("hidden");
  clearNotice(form);
  el("resetPwUser").textContent = user.username;
  openDialog("resetPwDialog");
}

export function initSettings() {
  const addUser = bindCollapsible("addUserToggle", "adminUserForm");

  el("changePwForm").onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(event.target);
    const form = new FormData(event.target);
    try {
      await api("/admin/auth/password", {
        method: "PUT",
        body: JSON.stringify({
          currentPassword: form.get("currentPassword"),
          newPassword: form.get("newPassword"),
        }),
      });
      event.target.reset();
      // The server invalidated every session, so this one ends too. The message
      // rides along to the login page's own notice line rather than a toast.
      setSignOutStatus("Password changed - sign in again with the new password.");
      setToken("");
    } catch (e) {
      notifyError(e.message, { form: event.target });
    }
  };

  el("adminUserForm").onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(event.target);
    const form = new FormData(event.target);
    try {
      await api("/admin/auth/users", {
        method: "POST",
        body: JSON.stringify({
          username: form.get("username"),
          password: form.get("password"),
          role: form.get("role"),
        }),
      });
      event.target.reset();
      addUser.setOpen(false); // collapse again - the new row shows up below
      loadSettings();
    } catch (e) {
      notifyError(e.message, { form: event.target });
    }
  };

  el("resetPwForm").onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const username = el("resetPwUser").textContent;
    const typed = new FormData(form).get("newPassword");
    // Blank means "pick one for me"; the value is shown once when we do.
    const generated = !typed;
    const newPassword = generated ? randomPassword() : typed;
    try {
      const users = await api("/admin/auth/users");
      const target = users.data.find((u) => u.username === username);
      if (!target) throw new Error("User not found");
      await api(`/admin/auth/users/${target.id}`, {
        method: "PUT",
        body: JSON.stringify({ newPassword }),
      });
      clearUsersNotices();
      loadSettings();
      // A password the admin typed is already known; only a generated one has
      // to be handed over, and only here (the form gives way to the result).
      if (!generated) return closeDialog("resetPwDialog");
      el("resetPwResultUser").textContent = username;
      el("resetPwResultValue").textContent = newPassword;
      form.classList.add("hidden");
      el("resetPwResult").classList.remove("hidden");
    } catch (e) {
      notifyError(e.message, { form });
    }
  };

  el("resetPwCancel").onclick = () => closeDialog("resetPwDialog");
  el("resetPwDone").onclick = () => closeDialog("resetPwDialog");
}
