/** Small DOM / rendering helpers shared by every tab. */

import { clearNotice, notifyError } from "./notice.js";

export function el(id) {
  return document.getElementById(id);
}

export function openDialog(id) {
  el(id).showModal();
}

export function closeDialog(id) {
  el(id).close();
}

/**
 * Render rows into a table's <tbody>.
 *
 * Each cell gets a `data-label` taken from its column header so the CSS can
 * stack rows into labeled cards on narrow screens. Cells built from elements
 * (action buttons) are placed as-is; the headerless last column is tagged
 * `cell-actions`.
 */
export function fillTable(tableId, rows) {
  const table = el(tableId);
  const tbody = table.querySelector("tbody");
  const headers = [...table.querySelectorAll("thead th")].map((th) => th.textContent.trim());
  tbody.innerHTML = "";

  if (rows.length === 0) {
    renderMessage(tbody, headers.length, "No data yet.");
    return;
  }

  for (const row of rows) {
    const tr = document.createElement("tr");
    row.forEach((cell, index) => {
      const td = document.createElement("td");
      if (cell instanceof HTMLElement) td.appendChild(cell);
      else td.textContent = cell ?? "";
      const label = headers[index] ?? "";
      if (label) td.dataset.label = label;
      else if (index > 0) td.classList.add("cell-actions");
      tr.appendChild(td);
    });
    tbody.appendChild(tr);
  }
}

/** Fill a table body with a single message row (empty state / load error). */
export function fillTableMessage(tableId, message, { error = false } = {}) {
  const table = el(tableId);
  const tbody = table.querySelector("tbody");
  const headers = table.querySelectorAll("thead th").length;
  tbody.innerHTML = "";
  renderMessage(tbody, headers, message, error);
}

function renderMessage(tbody, columnCount, message, error = false) {
  const tr = document.createElement("tr");
  const td = document.createElement("td");
  td.className = error ? "cell-message error" : "cell-message";
  td.colSpan = Math.max(columnCount, 1);
  td.textContent = message;
  tr.appendChild(td);
  tbody.appendChild(tr);
}

/** Inline state chip, e.g. an enabled flag: `badge("ON", "on")`. */
export function badge(text, className) {
  const span = document.createElement("span");
  span.className = className ? `badge ${className}` : "badge";
  span.textContent = text;
  return span;
}

/**
 * Monospace cell for machine-ish values such as an upstream URL: the text
 * wraps inside the column instead of stretching the table, and stays readable
 * when a value is long (see .cell-mono in components.css).
 */
export function monoCell(value) {
  const span = document.createElement("span");
  span.className = "cell-mono";
  span.textContent = value;
  return span;
}

/**
 * HTTP status cell: a pill whose colour encodes the outcome, so a failing
 * request stands out in the usage and dashboard tables. Non-numeric statuses
 * (a request that never reached the upstream) fall back to the error style.
 */
export function statusPill(status) {
  const span = document.createElement("span");
  if (status === null || status === undefined || status === "") {
    span.className = "muted";
    span.textContent = "-";
    return span;
  }
  const group = `${Math.trunc(Number(status) / 100)}xx`;
  const known = ["2xx", "3xx", "4xx", "5xx"].includes(group);
  span.className = `pill status-${known ? group : "err"}`;
  span.textContent = String(status);
  return span;
}

/**
 * Ask for confirmation in the shared modal dialog, then run `action`.
 *
 * The dialog owns the whole interaction: it stays open while the action runs
 * (the confirm button is disabled so a double click cannot fire twice), closes
 * on success, and on failure shows the server's message inline - so a blocked
 * delete (the last enabled admin, say) can be retried or cancelled from where
 * it was asked for instead of leaving a toast behind a closed dialog.
 */
export function askConfirm({ title, message = "", confirmLabel = "Delete", action }) {
  const form = el("confirmDialogForm");
  const confirm = el("confirmDialogOk");

  el("confirmDialogTitle").textContent = title;
  const hint = el("confirmDialogMessage");
  hint.textContent = message;
  hint.classList.toggle("hidden", !message);
  confirm.textContent = confirmLabel;
  clearNotice(form);

  form.onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(form);
    confirm.disabled = true;
    try {
      await action();
      closeDialog("confirmDialog");
    } catch (e) {
      notifyError(e.message, { form });
    } finally {
      confirm.disabled = false;
    }
  };

  el("confirmDialogCancel").onclick = () => closeDialog("confirmDialog");
  openDialog("confirmDialog");
}

/**
 * Both button helpers build mutating row actions (edit, reset, enable/disable,
 * delete), so they tag their output `admin-only`: for a read-only account the
 * CSS in base.css hides them and the row simply has no actions.
 */
export function delButton(onClick) {
  const btn = document.createElement("button");
  btn.textContent = "Delete";
  btn.className = "danger small admin-only";
  btn.onclick = onClick;
  return btn;
}

export function smallButton(text, onClick, className) {
  const btn = document.createElement("button");
  btn.textContent = text;
  btn.className = "small admin-only";
  if (className) btn.classList.add(className);
  btn.onclick = onClick;
  return btn;
}

const COLLAPSE_MS = 180;

/**
 * Wire a toggle button to a collapsible form. The form starts hidden, the
 * button gets a caret that flips while `aria-expanded` tracks the state, and
 * opening/closing is animated.
 *
 * Returns { setOpen } so callers can collapse the form again after saving.
 */
export function bindCollapsible(toggleId, targetId) {
  const toggle = el(toggleId);
  const target = el(targetId);
  // The gap below the form has to animate too, otherwise the content below
  // jumps by that amount the moment the form finishes collapsing.
  const gap = parseFloat(getComputedStyle(target).marginBottom) || 0;

  toggle.classList.add("collapse-toggle");

  const caret = document.createElement("span");
  caret.className = "caret";
  caret.setAttribute("aria-hidden", "true");
  caret.textContent = "▾";
  toggle.appendChild(caret);

  let open = false;
  let animation = null;

  /** Apply the final (non-animated) state. */
  function render() {
    target.classList.toggle("hidden", !open);
    target.style.height = "";
    target.style.overflow = "";
    target.style.marginBottom = "";
    toggle.setAttribute("aria-expanded", String(open));
  }

  function setOpen(next, animate = true) {
    if (next === open) return;
    open = next;
    toggle.setAttribute("aria-expanded", String(open));

    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (!animate || reducedMotion) {
      animation?.cancel();
      animation = null;
      render();
      if (open) target.querySelector("input, select")?.focus();
      return;
    }

    // Height is read before unhiding (0 while closed) so a click in the middle
    // of a running animation continues from wherever it currently is.
    const startHeight = target.getBoundingClientRect().height;
    animation?.cancel();
    if (open) target.classList.remove("hidden");
    const endHeight = open ? target.scrollHeight : 0;
    target.style.overflow = "hidden";

    animation = target.animate(
      {
        height: [`${startHeight}px`, `${endHeight}px`],
        marginBottom: open ? ["0px", `${gap}px`] : [`${gap}px`, "0px"],
      },
      { duration: COLLAPSE_MS, easing: "ease" },
    );
    animation.onfinish = () => {
      animation = null;
      render();
      if (open) target.querySelector("input, select")?.focus();
    };
  }

  toggle.onclick = () => setOpen(!open);
  render();

  return { setOpen };
}

/** Group row action buttons so they wrap cleanly (incl. mobile cards). */
export function wrapButtons(...buttons) {
  const span = document.createElement("span");
  span.className = "row-actions";
  for (const button of buttons) span.appendChild(button);
  return span;
}

/**
 * Wire an info icon to the popover it opens. The popover is taken out of the
 * page flow (`.info-pop`), so a long explanation only costs space while it is
 * shown: clicking the icon toggles it, clicking outside it or pressing Escape
 * closes it.
 */
export function bindInfoPopover(toggleId, popoverId) {
  const toggle = el(toggleId);
  const popover = el(popoverId);

  function setOpen(open) {
    popover.classList.toggle("hidden", !open);
    toggle.setAttribute("aria-expanded", String(open));
  }

  toggle.onclick = (event) => {
    // The click must not reach the document listener below, which would close
    // the popover this very click just opened.
    event.stopPropagation();
    setOpen(popover.classList.contains("hidden"));
  };

  document.addEventListener("click", (event) => {
    if (!popover.classList.contains("hidden") && !popover.contains(event.target)) setOpen(false);
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && !popover.classList.contains("hidden")) {
      setOpen(false);
      toggle.focus();
    }
  });

  return { setOpen };
}
