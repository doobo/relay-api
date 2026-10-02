/**
 * API keys tab.
 *
 * Create and edit both go through one modal dialog with labeled fields. Token
 * quota limits are entered and displayed in millions (1 = 1,000,000 tokens);
 * the API stores absolute token counts, so the form converts on the way in/out.
 */

import { api } from "../core/api.js";
import {
  askConfirm,
  closeDialog,
  delButton,
  el,
  fillTable,
  fillTableMessage,
  openDialog,
  smallButton,
  wrapButtons,
} from "../core/dom.js";
import { fmtTime } from "../core/format.js";
import { clearNotice, notifyError } from "../core/notice.js";

/** One quota token-unit = 1,000,000 tokens. */
const TOKEN_UNIT = 1_000_000;

/** Request-count quota inputs (plain counts). */
const REQUEST_FIELDS = ["dailyRequestLimit", "weeklyRequestLimit", "monthlyRequestLimit"];
/** Token quota inputs (millions in the form, absolute tokens in the API). */
const TOKEN_FIELDS = ["dailyTokenLimit", "weeklyTokenLimit", "monthlyTokenLimit"];

/** dailyRequestLimit -> daily_request_limit (key rows use snake_case). */
function snakeCase(name) {
  return name.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`);
}

function toNonNegative(value) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

/** Form values -> API payload (token fields converted from millions). */
function limitsFromForm(form) {
  const data = new FormData(form);
  const body = {};
  for (const name of REQUEST_FIELDS) body[name] = Math.trunc(toNonNegative(data.get(name)));
  for (const name of TOKEN_FIELDS) {
    body[name] = Math.round(toNonNegative(data.get(name)) * TOKEN_UNIT);
  }
  return body;
}

/** Key row (absolute tokens) -> form values (millions). */
function fillLimits(form, key) {
  for (const name of REQUEST_FIELDS) {
    form.elements[name].value = String(key[snakeCase(name)] ?? 0);
  }
  for (const name of TOKEN_FIELDS) {
    form.elements[name].value = String((key[snakeCase(name)] ?? 0) / TOKEN_UNIT);
  }
}

/** Absolute tokens -> "1.5M"; 0 -> ∞ (unlimited). */
function fmtMillions(tokens) {
  if (!(tokens > 0)) return "∞";
  const millions = tokens / TOKEN_UNIT;
  const text = Number.isInteger(millions)
    ? String(millions)
    : millions.toFixed(3).replace(/0+$/, "").replace(/\.$/, "");
  return `${text}M`;
}

/**
 * Requests render as plain grouped counts so they can never be mistaken for the
 * million-suffixed token values ("1,000,000/1.5M" rather than "1.0M/1.5M").
 * 0 renders as ∞ (unlimited).
 */
function fmtRequests(requests) {
  return requests > 0 ? requests.toLocaleString() : "∞";
}

/** "100/1.5M" - units: requests, then millions of tokens. */
function limitPair(requests, tokens) {
  return `${fmtRequests(requests)}/${fmtMillions(tokens)}`;
}

function limitsSummary(key) {
  return [
    `D ${limitPair(key.daily_request_limit, key.daily_token_limit)}`,
    `W ${limitPair(key.weekly_request_limit, key.weekly_token_limit)}`,
    `M ${limitPair(key.monthly_request_limit, key.monthly_token_limit)}`,
  ].join(" · ");
}

export async function loadKeys() {
  try {
    const data = await api("/admin/api-keys");
    fillTable(
      "keysTable",
      data.data.map((k) => [
        k.id,
        k.name,
        k.prefix,
        k.scope,
        k.rate_limit,
        limitsSummary(k),
        k.last_used_at ? fmtTime(k.last_used_at) : "never",
        wrapButtons(
          smallButton("Edit", () => openKeyDialog("edit", k)),
          delButton(() =>
            askConfirm({
              title: `Delete API key '${k.name}'?`,
              message: "Clients using this key lose access immediately. This cannot be undone.",
              action: async () => {
                await api(`/admin/api-keys/${k.id}`, { method: "DELETE" });
                loadKeys();
              },
            }),
          ),
        ),
      ]),
    );
  } catch (e) {
    fillTableMessage("keysTable", `Failed to load: ${e.message}`, { error: true });
  }
}

function openKeyDialog(mode, key) {
  const form = el("keyForm");
  form.reset();
  form.dataset.mode = mode;
  form.dataset.id = key ? String(key.id) : "";
  el("keyEnabledField").classList.toggle("hidden", mode !== "edit");
  el("keyDialogTitle").textContent = mode === "edit" ? `Edit API key: ${key.name}` : "Add API key";
  el("keyDialogSubmit").textContent = mode === "edit" ? "Save" : "Create";
  if (mode === "create") el("newKeyBanner").classList.add("hidden");

  if (key) {
    form.elements.name.value = key.name;
    form.elements.scope.value = key.scope;
    form.elements.rateLimit.value = String(key.rate_limit);
    form.elements.enabled.checked = Boolean(key.enabled);
    fillLimits(form, key);
  }
  openDialog("keyDialog");
}

export function initKeys() {
  el("keyCreateBtn").onclick = () => openKeyDialog("create");
  el("keyDialogCancel").onclick = () => closeDialog("keyDialog");

  el("keyForm").onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const data = new FormData(form);
    const mode = form.dataset.mode;
    const body = {
      name: data.get("name"),
      scope: data.get("scope"),
      rateLimit: Math.trunc(toNonNegative(data.get("rateLimit"))) || 60,
      ...limitsFromForm(form),
    };
    if (mode === "edit") body.enabled = form.elements.enabled.checked;

    try {
      if (mode === "create") {
        const created = await api("/admin/api-keys", {
          method: "POST",
          body: JSON.stringify(body),
        });
        closeDialog("keyDialog");
        const banner = el("newKeyBanner");
        banner.classList.remove("hidden");
        banner.textContent = `New key (copy now, shown once): ${created.key}`;
      } else {
        await api(`/admin/api-keys/${form.dataset.id}`, {
          method: "PUT",
          body: JSON.stringify(body),
        });
        closeDialog("keyDialog");
      }
      loadKeys();
    } catch (e) {
      notifyError(e.message, { form });
    }
  };
}
