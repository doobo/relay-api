/**
 * Single surface for user-visible errors (no browser alert popups).
 *
 * - With a form (submit handlers, dialogs): the message is rendered inline in
 *   that form, right above its action row, so the user sees it where they were
 *   typing and the dialog stays open for a retry.
 * - Without one (row actions like delete/enable): a toast floats over the
 *   page, and inside the open <dialog> when there is one, since modal dialogs
 *   live in the top layer and would otherwise cover a page-level toast.
 */

const INLINE_CLASS = "notice";

/**
 * The element a form's errors go into: a slot the markup already provides
 * (`.notice` as a direct child, e.g. the login card's reserved line) or one
 * created just above the action row on first use.
 */
function inlineSlot(form) {
  let slot = form.querySelector(":scope > .notice");
  if (!slot) {
    slot = document.createElement("div");
    slot.className = `${INLINE_CLASS} banner danger`;
    slot.setAttribute("role", "alert");
    const actions = form.querySelector(":scope > button[type=submit], :scope > .dialog-actions");
    form.insertBefore(slot, actions ?? null);
  }
  return slot;
}

export function notifyError(message, { form } = {}) {
  if (form) {
    const slot = inlineSlot(form);
    slot.textContent = message;
    // `error` lets a form-provided slot colour a failure differently from an
    // informational message it also carries (see .login-notice in layout.css).
    slot.classList.add("error");
    slot.classList.remove("hidden");
    return;
  }
  showToast(message, "danger");
}

/** Non-error confirmation, e.g. "password changed, sign in again". */
export function notifyInfo(message) {
  showToast(message, "info");
}

function showToast(message, kind) {
  const host = document.querySelector("dialog[open]") ?? document.body;
  let toast = host.querySelector(":scope > .toast");
  if (!toast) {
    toast = document.createElement("div");
    toast.className = "toast";
    toast.setAttribute("role", "alert");
    toast.title = "Click to dismiss";
    toast.onclick = () => toast.classList.remove("visible");
    host.appendChild(toast);
  }
  toast.classList.toggle("info", kind === "info");
  toast.textContent = message;
  toast.classList.add("visible");

  clearTimeout(toast.dismissTimer);
  toast.dismissTimer = setTimeout(() => toast.classList.remove("visible"), 6000);
}

/** Hide the inline error of a form (e.g. as soon as the user edits a field). */
export function clearNotice(form) {
  form?.querySelector(":scope > .notice")?.classList.add("hidden");
}
