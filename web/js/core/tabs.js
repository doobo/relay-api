/**
 * Tab state: owns which tab is visible and which loader belongs to it, so any
 * module (entry, user menu, ...) can switch tabs without importing the app
 * entry point.
 */

const loaders = new Map();

/** Register the data loader for a tab name (matches nav `data-tab`). */
export function registerTab(name, loader) {
  loaders.set(name, loader);
}

export function activeTabName() {
  return document.querySelector(".tabs button.active")?.dataset.tab ?? null;
}

/** Tab name from the URL hash, or null when it names no known tab. */
export function tabFromHash() {
  const name = location.hash.replace(/^#/, "").trim().toLowerCase();
  if (!name) return null;
  return document.querySelector(`.tabs button[data-tab="${name}"]`) ? name : null;
}

/** Mark one tab (and its nav button) active without loading its data. */
function applyActive(name) {
  for (const btn of document.querySelectorAll(".tabs button")) {
    btn.classList.toggle("active", btn.dataset.tab === name);
  }
  for (const tab of document.querySelectorAll(".tab")) {
    tab.classList.toggle("active", tab.id === `tab-${name}`);
  }
}

/** Show one tab, mirror it in the URL hash and load its data. */
export function selectTab(name) {
  applyActive(name);
  // Keep the hash in sync so a refresh (or a shared link) reopens this tab. A
  // plain assignment is deliberate: it lets the browser's back button walk
  // through tabs, and the guard below stops the resulting hashchange from
  // loading the same tab twice.
  if (location.hash !== `#${name}`) location.hash = name;
  loaders.get(name)?.();
}

/**
 * Apply the tab named by the current URL hash without triggering a load. Call
 * this early (before the first refresh) so a reload lands on the same tab.
 */
export function restoreTabFromHash() {
  const name = tabFromHash();
  if (!name) return null;
  applyActive(name);
  return name;
}

/** Reload the visible tab's data only; other tabs load on demand. */
export function refreshActiveTab() {
  const name = activeTabName();
  if (name) loaders.get(name)?.();
}

/** Wire the nav buttons to selectTab and follow hash changes (back/forward). */
export function initTabBar() {
  for (const btn of document.querySelectorAll(".tabs button")) {
    btn.addEventListener("click", () => selectTab(btn.dataset.tab));
  }
  window.addEventListener("hashchange", () => {
    const name = tabFromHash();
    if (name && name !== activeTabName()) selectTab(name);
  });
}
