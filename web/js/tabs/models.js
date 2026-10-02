/** Models tab: aliases mapped onto providers. */

import { api } from "../core/api.js";
import { askConfirm, badge, bindCollapsible, closeDialog, delButton, el, fillTable, fillTableMessage, openDialog, smallButton, wrapButtons } from "../core/dom.js";
import { clearNotice, notifyError } from "../core/notice.js";
import { cachedProviders, fetchProviders } from "./providers.js";

/** Fill a datalist with provider options (`name` value, `#id · type` label). */
function fillProviderDatalist(datalistId, list) {
  const datalist = el(datalistId);
  datalist.innerHTML = "";
  for (const p of list) {
    const opt = document.createElement("option");
    opt.value = p.name;
    opt.label = `#${p.id} · ${p.type}`;
    datalist.appendChild(opt);
  }
}

function matchesProvider(p, query) {
  return (
    p.name.toLowerCase().includes(query) ||
    p.type.toLowerCase().includes(query) ||
    String(p.id) === query
  );
}

/**
 * Local search: typing filters the datalist, and selecting an entry fills the
 * numeric provider id field.
 */
function bindProviderPicker(searchInput, providerIdInput, datalistId) {
  searchInput.addEventListener("input", (e) => {
    const query = e.target.value.trim().toLowerCase();
    const list = cachedProviders();
    fillProviderDatalist(datalistId, query ? list.filter((p) => matchesProvider(p, query)) : list);
  });

  searchInput.addEventListener("change", (e) => {
    const value = e.target.value.trim().toLowerCase();
    const selected = cachedProviders().find((p) => p.name.toLowerCase() === value);
    if (selected) providerIdInput.value = selected.id;
  });
}

function resolveProviderId(searchValue, fallbackId) {
  const value = (searchValue || "").trim().toLowerCase();
  const picked = cachedProviders().find((p) => p.name.toLowerCase() === value);
  return picked ? picked.id : Number(fallbackId);
}

export async function loadModels() {
  try {
    // Providers are resolved here to show names and power the searchable
    // picker - only the current tab's APIs are called.
    const providers = await fetchProviders();
    const byId = new Map(providers.map((p) => [p.id, p]));
    fillProviderDatalist("providerOptions", providers);
    fillProviderDatalist("providerEditOptions", providers);

    const data = await api("/admin/models");
    fillTable(
      "modelsTable",
      data.data.map((m) => {
        const provider = byId.get(m.provider_id);
        return [
          m.id,
          m.name,
          provider ? `${provider.name} (#${provider.id})` : `#${m.provider_id} (missing)`,
          m.upstream_model,
          badge(m.enabled ? "ON" : "OFF", m.enabled ? "on" : "off"),
          wrapButtons(
            smallButton("Edit", () => openModelEdit(m)),
            delButton(() =>
              askConfirm({
                title: `Delete model alias '${m.name}'?`,
                message: "Clients asking for this alias start getting a 404. This cannot be undone.",
                action: async () => {
                  await api(`/admin/models/${m.id}`, { method: "DELETE" });
                  loadModels();
                },
              }),
            ),
          ),
        ];
      }),
    );
  } catch (e) {
    fillTableMessage("modelsTable", `Failed to load: ${e.message}`, { error: true });
  }
}

// Model edit dialog (prefilled with the row's provider via the same
// search-by-name + numeric-id pairing used by the create form).
function openModelEdit(m) {
  const form = el("modelEditForm");
  form.dataset.id = m.id;
  el("modelEditName").textContent = m.name;
  form.elements.name.value = m.name;
  const provider = cachedProviders().find((p) => p.id === m.provider_id);
  form.elements.providerSearch.value = provider ? provider.name : "";
  form.elements.providerId.value = m.provider_id;
  form.elements.upstreamModel.value = m.upstream_model;
  form.elements.enabled.checked = Boolean(m.enabled);
  openDialog("modelEditDialog");
}

export function initModels() {
  const addModel = bindCollapsible("modelFormToggle", "modelForm");
  const createForm = el("modelForm");
  bindProviderPicker(createForm.elements.providerSearch, createForm.elements.providerId, "providerOptions");

  createForm.onsubmit = async (event) => {
    event.preventDefault();
    clearNotice(createForm);
    const form = new FormData(event.target);
    const providerId = resolveProviderId(form.get("providerSearch"), form.get("providerId"));
    if (!providerId) {
      notifyError("Pick a provider by name or enter a provider id", { form: createForm });
      return;
    }
    try {
      await api("/admin/models", {
        method: "POST",
        body: JSON.stringify({
          name: form.get("name"),
          providerId,
          upstreamModel: form.get("upstreamModel"),
        }),
      });
      event.target.reset();
      addModel.setOpen(false);
      loadModels();
    } catch (e) {
      notifyError(e.message, { form: createForm });
    }
  };

  const editForm = el("modelEditForm");
  bindProviderPicker(editForm.elements.providerSearch, editForm.elements.providerId, "providerEditOptions");

  editForm.onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const providerId = resolveProviderId(
      form.elements.providerSearch.value,
      form.elements.providerId.value,
    );
    if (!providerId) {
      notifyError("Pick a provider by name or enter a provider id", { form });
      return;
    }
    try {
      await api(`/admin/models/${form.dataset.id}`, {
        method: "PUT",
        body: JSON.stringify({
          name: form.elements.name.value,
          providerId,
          upstreamModel: form.elements.upstreamModel.value,
          enabled: form.elements.enabled.checked,
        }),
      });
      closeDialog("modelEditDialog");
      loadModels();
    } catch (e) {
      notifyError(e.message, { form });
    }
  };

  el("modelEditCancel").onclick = () => closeDialog("modelEditDialog");
}
