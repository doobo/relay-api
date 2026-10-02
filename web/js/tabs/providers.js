/**
 * Providers tab. Also owns the shared provider cache other tabs read from
 * (models need provider names to resolve ids).
 */

import { api } from "../core/api.js";
import { askConfirm, badge, bindCollapsible, closeDialog, delButton, el, fillTable, fillTableMessage, openDialog, smallButton, wrapButtons } from "../core/dom.js";
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
        p.type,
        p.base_url,
        p.api_key_masked || "(none)",
        badge(p.enabled ? "ON" : "OFF", p.enabled ? "on" : "off"),
        wrapButtons(
          smallButton("Edit", () => openProviderEdit(p)),
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

function openProviderEdit(p) {
  const form = el("providerEditForm");
  form.dataset.id = p.id;
  el("providerEditName").textContent = p.name;
  form.elements.name.value = p.name;
  form.elements.type.value = p.type;
  form.elements.baseUrl.value = p.base_url;
  form.elements.apiKey.value = "";
  form.elements.apiKey.placeholder = p.has_api_key
    ? "new API key (leave blank to keep current)"
    : "API key (optional)";
  form.elements.enabled.checked = Boolean(p.enabled);
  openDialog("providerEditDialog");
}

export function initProviders() {
  const addProvider = bindCollapsible("providerFormToggle", "providerForm");

  el("providerEditForm").onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const id = form.dataset.id;
    const body = {
      name: form.elements.name.value,
      type: form.elements.type.value,
      baseUrl: form.elements.baseUrl.value,
      enabled: form.elements.enabled.checked,
    };
    const newKey = form.elements.apiKey.value.trim();
    if (newKey) body.apiKey = newKey; // only send when rotating
    try {
      await api(`/admin/providers/${id}`, { method: "PUT", body: JSON.stringify(body) });
      closeDialog("providerEditDialog");
      invalidateProviders();
      loadProviders();
    } catch (e) {
      notifyError(e.message, { form });
    }
  };

  el("providerEditCancel").onclick = () => closeDialog("providerEditDialog");

  el("providerForm").onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(event.target);
    const form = new FormData(event.target);
    try {
      await api("/admin/providers", {
        method: "POST",
        body: JSON.stringify({
          name: form.get("name"),
          type: form.get("type"),
          baseUrl: form.get("baseUrl"),
          apiKey: form.get("apiKey") || null,
        }),
      });
      event.target.reset();
      addProvider.setOpen(false);
      invalidateProviders();
      loadProviders();
    } catch (e) {
      notifyError(e.message, { form: event.target });
    }
  };
}
