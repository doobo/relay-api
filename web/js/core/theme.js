/**
 * Appearance: the palettes live in base.css as `:root` (dark) plus
 * `html[data-theme="light"|"black"]` blocks.
 *
 * The choice is a browser preference, not a server-side setting, so it stays
 * in localStorage. "auto" follows the OS scheme; the extra
 * `data-theme-choice` attribute keeps the resolved theme distinguishable from
 * what the user actually picked in the menu.
 *
 * The picker appears twice (login page and header); both carry
 * `data-theme-select` and are driven from here, so they can never drift.
 *
 * The same setting is applied once before first paint by an inline script in
 * index.html (avoids a flash of the wrong palette) - keep THEME_KEY in sync.
 */

const THEMES = ["auto", "dark", "light", "black"];
const THEME_KEY = "mini-api-gateway.theme";
const LIGHT_QUERY = "(prefers-color-scheme: light)";

let choice = storedChoice();

function storedChoice() {
  try {
    const value = localStorage.getItem(THEME_KEY);
    return THEMES.includes(value) ? value : "auto";
  } catch {
    // Private mode / storage disabled: the default palette still works.
    return "auto";
  }
}

function systemTheme() {
  return window.matchMedia(LIGHT_QUERY).matches ? "light" : "dark";
}

/** Resolve the stored choice to a concrete palette name. */
function resolveTheme(name = choice) {
  return name === "auto" ? systemTheme() : name;
}

/** Apply a palette (and remember the choice) across the document. */
export function applyTheme(name) {
  choice = THEMES.includes(name) ? name : "auto";
  const theme = resolveTheme(choice);
  const root = document.documentElement;
  root.dataset.theme = theme;
  root.dataset.themeChoice = choice;
  // Native widgets (scrollbars, checkboxes, the login password reveal) follow
  // color-scheme rather than our CSS variables.
  root.style.colorScheme = theme === "light" ? "light" : "dark";
  syncThemeColor(theme);

  syncSelects();

  try {
    localStorage.setItem(THEME_KEY, choice);
  } catch {
    // Ignore: the setting just won't survive a reload.
  }
}

/** Keep the mobile browser chrome in step with the palette's background. */
function syncThemeColor(theme) {
  const meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) return;
  const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
  if (bg) meta.setAttribute("content", bg);
}

function syncSelects() {
  for (const select of document.querySelectorAll("[data-theme-select]")) {
    select.value = choice;
  }
}

/** Wire every picker and follow the OS while the choice is "auto". */
export function initTheme() {
  for (const select of document.querySelectorAll("[data-theme-select]")) {
    select.value = choice;
    select.addEventListener("change", () => applyTheme(select.value));
  }

  // Only matters for "auto": picking a palette pins the theme explicitly.
  window.matchMedia(LIGHT_QUERY).addEventListener("change", () => {
    if (choice === "auto") applyTheme("auto");
  });

  applyTheme(choice);
}
