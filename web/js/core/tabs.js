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

/** Show one tab and load its data. */
export function selectTab(name) {
  for (const btn of document.querySelectorAll(".tabs button")) {
    btn.classList.toggle("active", btn.dataset.tab === name);
  }
  for (const tab of document.querySelectorAll(".tab")) {
    tab.classList.toggle("active", tab.id === `tab-${name}`);
  }
  loaders.get(name)?.();
}

/** Reload the visible tab's data only; other tabs load on demand. */
export function refreshActiveTab() {
  const name = activeTabName();
  if (name) loaders.get(name)?.();
}

/** Wire the nav buttons to selectTab. */
export function initTabBar() {
  for (const btn of document.querySelectorAll(".tabs button")) {
    btn.addEventListener("click", () => selectTab(btn.dataset.tab));
  }
}
