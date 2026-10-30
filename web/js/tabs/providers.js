/**
 * Providers tab. Also owns the shared provider cache other tabs read from
 * (models need provider names to resolve ids).
 */

import { api } from "../core/api.js";
import { askConfirm, badge, closeDialog, delButton, el, fillTable, fillTableMessage, openDialog, smallButton, wrapButtons } from "../core/dom.js";
import { clearNotice, notifyError } from "../core/notice.js";

let providers = [];

/** Providers, fetched once and reused until invalidated. */
export async function fetchProviders(force = false) {
  if (force || providers.length === 0) {
    const data = await api("/admin/providers");
    providers = data.data;
  }
  return providers;
}

/** Synchronous view of the cache for event handlers that cannot await. */
export function cachedProviders() {
  return providers;
}

export function invalidateProviders() {
  providers = [];
}

export async function loadProviders() {
  try {
    const list = await fetchProviders(true);
    fillTable(
      "providersTable",
      list.map((p) => [
        p.id,
        p.name,
        p.reasoning_replay ? `${p.type} + replay` : p.type,
        p.base_url,
        String(Object.keys(p.headers || {}).length),
        p.api_key_masked || "(none)",
        badge(p.enabled ? "ON" : "OFF", p.enabled ? "on" : "off"),
        wrapButtons(
          smallButton("Edit", () => openProviderDialog("edit", p)),
          delButton(() =>
            askConfirm({
              title: `Delete provider '${p.name}'?`,
              // Deleting is refused while aliases still point here, so say that
              // rather than promising the aliases would break.
              message:
                "Its upstream URL and key are removed. Model aliases that still point at it block the delete. This cannot be undone.",
              action: async () => {
                await api(`/admin/providers/${p.id}`, { method: "DELETE" });
                invalidateProviders();
                loadProviders();
              },
            }),
          ),
        ),
      ]),
    );
  } catch (e) {
    fillTableMessage("providersTable", `Failed to load: ${e.message}`, { error: true });
  }
}

// One dialog serves add and edit. Creating starts from the form defaults;
// editing fills every field and keeps the API key blank (blank = unchanged).
function openProviderDialog(mode, p) {
  const form = el("providerEditForm");
  form.reset();
  form.dataset.mode = mode;
  form.dataset.id = p ? String(p.id) : "";
  form.elements.enabled.checked = true;
  el("providerDialogTitle").textContent = mode === "edit" && p ? `Edit provider ${p.name}` : "Add provider";
  el("providerEditSubmit").textContent = mode === "edit" ? "Save" : "Create";

  if (mode === "edit" && p) {
    form.elements.name.value = p.name;
    form.elements.type.value = p.type;
    form.elements.baseUrl.value = p.base_url;
    form.elements.apiKey.placeholder = p.has_api_key
      ? "new API key (leave blank to keep current)"
      : "API key (optional)";
    form.elements.headers.value = Object.keys(p.headers || {}).length
      ? JSON.stringify(p.headers)
      : "";
    form.elements.reasoningReplay.checked = Boolean(p.reasoning_replay);
    form.elements.enabled.checked = Boolean(p.enabled);
  } else {
    form.elements.apiKey.placeholder = "API key (optional)";
  }
  openDialog("providerEditDialog");
}

// parseHeaderInput accepts the JSON object shown in the form; a blank value
// means no custom headers.
function parseHeaderInput(raw) {
  const text = String(raw || "").trim();
  if (!text) return {};
  let parsed;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new Error("Headers must be a valid JSON object");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("Headers must be a JSON object");
  }
  for (const [name, value] of Object.entries(parsed)) {
    if (typeof value !== "string") {
      throw new Error(`Header '${name}' must have a string value`);
    }
  }
  return parsed;
}

export function initProviders() {
  el("providerCreateBtn").onclick = () => openProviderDialog("create");

  el("providerEditForm").onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const mode = form.dataset.mode;
    try {
      const body = {
        name: form.elements.name.value,
        type: form.elements.type.value,
        baseUrl: form.elements.baseUrl.value,
        headers: parseHeaderInput(form.elements.headers.value),
        reasoningReplay: form.elements.reasoningReplay.checked,
        enabled: form.elements.enabled.checked,
      };
      const newKey = form.elements.apiKey.value.trim();
      if (mode === "edit") {
        if (newKey) body.apiKey = newKey; // only send when rotating
        await api(`/admin/providers/${form.dataset.id}`, { method: "PUT", body: JSON.stringify(body) });
      } else {
        body.apiKey = newKey || null;
        await api("/admin/providers", { method: "POST", body: JSON.stringify(body) });
      }
      closeDialog("providerEditDialog");
      invalidateProviders();
      loadProviders();
    } catch (e) {
      notifyError(e.message, { form });
    }
  };

  el("providerEditCancel").onclick = () => closeDialog("providerEditDialog");
}
