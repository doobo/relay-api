/**
 * Admin UI entry point: registers the per-tab loaders, wires the tab bar, the
 * auth header and the dashboard refresh timer.
 */

import { getToken, onTokenChange } from "./core/api.js";
import { initAuth } from "./core/auth.js";
import { el } from "./core/dom.js";
import { loadRole } from "./core/perms.js";
import { initTabBar, refreshActiveTab, registerTab } from "./core/tabs.js";
import { initTheme } from "./core/theme.js";
import { initConfigs, loadConfigs } from "./tabs/configs.js";
import { loadDashboard } from "./tabs/dashboard.js";
import { initKeys, loadKeys } from "./tabs/keys.js";
import { loadLogs } from "./tabs/logs.js";
import { initModels, loadModels } from "./tabs/models.js";
import { initProviders, loadProviders } from "./tabs/providers.js";
import { initSettings, loadSettings } from "./tabs/settings.js";
import { initUsage, loadUsage } from "./tabs/usage.js";

/** Per-tab loaders. Switching tabs only fetches that tab's data. */
const tabs = {
  dashboard: loadDashboard,
  providers: loadProviders,
  models: loadModels,
  keys: loadKeys,
  configs: loadConfigs,
  usage: loadUsage,
  logs: loadLogs,
  settings: loadSettings,
};

function init() {
  for (const [name, loader] of Object.entries(tabs)) registerTab(name, loader);
  initTabBar();
  initTheme();
  initAuth();
  initTitleRefresh();
  initProviders();
  initModels();
  initKeys();
  initConfigs();
  initUsage();
  initSettings();

  // Logging in reloads the visible tab; clearing the session just hides the UI.
  onTokenChange((value) => {
    if (value) refreshWithRole();
  });

  // A session restored from sessionStorage does not change the token, so the
  // listener above never fires: fill the visible tab explicitly on mount.
  if (getToken()) {
    refreshWithRole();
    startDashboardPolling();
  }
}

/**
 * Resolve the account role before loading a tab. Doing it the other way round
 * would render admin controls - the role decides which ones a tab builds - for
 * however long /me takes to answer.
 */
async function refreshWithRole() {
  try {
    await loadRole();
  } catch {
    // /me failed. core/perms.js has already fallen back to read-only, which is
    // the safe thing to render. If the failure was a rejected session, the token
    // is gone by now and the login view is showing, so loading a tab would only
    // fire more requests that cannot succeed.
    if (!getToken()) return;
  }
  refreshActiveTab();
}

/** Clicking the header title reloads the visible tab's data. */
function initTitleRefresh() {
  const title = el("appTitle");
  title.onclick = () => refreshActiveTab();
  title.onkeydown = (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      refreshActiveTab();
    }
  };
}

/** The dashboard is the only tab that auto-refreshes. */
function startDashboardPolling() {
  setInterval(() => {
    const active = document.querySelector(".tabs button.active");
    if (active?.dataset.tab === "dashboard") loadDashboard();
  }, 30_000);
}

init();
