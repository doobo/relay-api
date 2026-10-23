/**
 * Usage tab: request log with server-side filters and incremental paging.
 *
 * It also hosts the per-key detail view: the API Keys tab jumps here with a
 * key selected, and the panel above the table shows that key's day/week/month
 * usage against its request and token quotas. The table below is the recorded
 * request detail for the selected key (or every key when none is selected).
 */

import { api } from "../core/api.js";
import { closeDialog, el, fillTable, fillTableMessage, monoCell, openDialog, statusPill } from "../core/dom.js";
import { fmtNumber, fmtTime } from "../core/format.js";
import { notifyError } from "../core/notice.js";
import { selectTab } from "../core/tabs.js";

const PAGE_SIZE = 50;

/** filter name -> input id (the name matches the query parameter). */
const FILTERS = {
  model: "usageFilterModel",
  provider: "usageFilterProvider",
  kind: "usageFilterKind",
  status: "usageFilterStatus",
};

let rows = [];
let offset = 0;
let hasMore = false;
/** id -> key name, filled when the key filter is populated. */
let keyNames = new Map();
/** Key selected by the API Keys tab before the usage tab loads. */
let pendingKeyID = "";

function queryString() {
  const params = new URLSearchParams();
  const keyID = el("usageFilterKey").value;
  if (keyID) params.set("api_key_id", keyID);
  for (const [name, id] of Object.entries(FILTERS)) {
    const value = el(id).value.trim();
    if (value) params.set(name, value);
  }
  params.set("limit", String(PAGE_SIZE));
  params.set("offset", String(offset));
  return params.toString();
}

function keyLabel(id) {
  if (!id) return "-";
  return keyNames.get(Number(id)) || `#${id}`;
}

function render() {
  fillTable(
    "usageTable",
    rows.map((u) => [
      fmtTime(u.created_at),
      u.request_id,
      u.kind,
      keyLabel(u.api_key_id),
      u.model || (u.api_config_id ? `config #${u.api_config_id}` : "-"),
      // Forwards have no provider: show where the request actually went. Older
      // rows predate the column and still fall back to a dash.
      u.provider || (u.target_url ? monoCell(u.target_url) : "-"),
      u.total_tokens,
      statusPill(u.status),
      `${u.latency_ms ?? "-"}ms`,
      detailButton(u.id),
    ]),
  );
  el("usageCount").textContent = rows.length === 0 ? "No matching requests." : `${rows.length} rows`;
  el("usageLoadMore").classList.toggle("hidden", !hasMore);
}

function detailButton(id) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = "Details";
  button.className = "secondary small";
  button.onclick = () => showUsageDetail(id);
  return button;
}

async function showUsageDetail(id) {
  try {
    const usage = await api(`/admin/usage/${id}`);
    const model = usage.model || (usage.api_config_id ? `config #${usage.api_config_id}` : "-");
    el("usageDetailMeta").textContent = [
      `Request: ${usage.request_id}`,
      `Time: ${fmtTime(usage.created_at)}`,
      `Kind: ${usage.kind}`,
      `API key: ${keyLabel(usage.api_key_id)}`,
      `Model/Config: ${model}`,
      `Provider: ${usage.provider || usage.target_url || "-"}`,
      `Status: ${usage.status ?? "-"}`,
      `Latency: ${usage.latency_ms ?? "-"}ms`,
      `Tokens: ${usage.total_tokens} (in ${usage.input_tokens} / out ${usage.output_tokens})`,
      `Stream: ${usage.stream ? "yes" : "no"}`,
      `Error: ${usage.error || "-"}`,
    ].join("\n");
    el("usageDetailRequestHeaders").textContent = formatHeaders(usage.request_headers);
    el("usageDetailUpstreamHeaders").textContent = formatHeaders(usage.upstream_headers);
    openDialog("usageDetailDialog");
  } catch (e) {
    notifyError(e.message);
  }
}

function formatHeaders(headers) {
  const entries = Object.entries(headers || {});
  if (entries.length === 0) return "(none recorded)";
  return JSON.stringify(Object.fromEntries(entries.sort(([a], [b]) => a.localeCompare(b))), null, 2);
}

async function fetchPage({ append }) {
  try {
    const data = await api(`/admin/usage?${queryString()}`);
    rows = append ? rows.concat(data.data) : data.data;
    offset = rows.length;
    hasMore = data.data.length === PAGE_SIZE;
    render();
  } catch (e) {
    rows = [];
    hasMore = false;
    fillTableMessage("usageTable", `Failed to load: ${e.message}`, { error: true });
    el("usageCount").textContent = "";
    el("usageLoadMore").classList.add("hidden");
  }
}

/** Fill the key dropdown, keeping the current selection when it still exists. */
async function loadKeyOptions() {
  const data = await api("/admin/api-keys");
  const select = el("usageFilterKey");
  const current = select.value;
  keyNames = new Map();
  select.innerHTML = '<option value="">all keys</option>';
  for (const key of data.data) {
    keyNames.set(key.id, key.name);
    const option = document.createElement("option");
    option.value = String(key.id);
    option.textContent = `${key.name} (${key.prefix}…)`;
    select.appendChild(option);
  }
  select.value = current;
  if (select.value !== current) {
    // The selected key was deleted while the tab was open.
    select.value = "";
  }
}

function periodText(period) {
  const requests = period.request_limit > 0 ? fmtNumber(period.request_limit) : "∞";
  const tokens = period.token_limit > 0 ? fmtNumber(period.token_limit) : "∞";
  return {
    requests: `${fmtNumber(period.used_requests)} / ${requests} req`,
    tokens: `${fmtNumber(period.used_tokens)} / ${tokens} tokens`,
  };
}

function renderKeySummary(stats) {
  const key = stats.key;
  el("keyUsageTitle").textContent = `API Key Usage · ${key.name}`;
  el("keyUsageMeta").textContent = [
    `Prefix ${key.prefix}`,
    `Scope ${key.scope}`,
    `RPM ${key.rate_limit}`,
    key.enabled ? "enabled" : "disabled",
    key.last_used_at ? `last used ${fmtTime(key.last_used_at)}` : "never used",
  ].join(" · ");

  for (const [name, period] of [
    ["Daily", stats.daily],
    ["Weekly", stats.weekly],
    ["Monthly", stats.monthly],
  ]) {
    const text = periodText(period);
    el(`keyUsage${name}Requests`).textContent = text.requests;
    el(`keyUsage${name}Tokens`).textContent = text.tokens;
  }
}

async function loadKeySummary() {
  const keyID = el("usageFilterKey").value;
  if (!keyID) {
    el("keyUsagePanel").classList.add("hidden");
    return;
  }
  try {
    const stats = await api(`/admin/api-keys/${keyID}/stats`);
    renderKeySummary(stats);
    el("keyUsagePanel").classList.remove("hidden");
  } catch (e) {
    el("keyUsagePanel").classList.add("hidden");
    notifyError(e.message);
  }
}

/** Called on tab entry: reload from the first page with the current filters. */
export async function loadUsage() {
  try {
    await loadKeyOptions();
  } catch {
    // Keep whatever options are already in the dropdown; the table fetch below
    // reports the real error.
  }
  if (pendingKeyID) {
    el("usageFilterKey").value = pendingKeyID;
    pendingKeyID = "";
  }
  offset = 0;
  await Promise.all([fetchPage({ append: false }), loadKeySummary()]);
}

/** Jump here from the API Keys tab with one key selected. */
export function openKeyUsage(keyID) {
  pendingKeyID = String(keyID);
  selectTab("usage");
}

export function initUsage() {
  el("usageFilters").onsubmit = (event) => {
    event.preventDefault();
    loadUsage();
  };
  el("usageFilterReset").onclick = () => {
    for (const id of Object.values(FILTERS)) el(id).value = "";
    el("usageFilterKey").value = "";
    el("keyUsagePanel").classList.add("hidden");
    loadUsage();
  };
  el("usageFilterKey").onchange = () => loadUsage();
  el("keyUsageClear").onclick = () => {
    el("usageFilterKey").value = "";
    el("keyUsagePanel").classList.add("hidden");
    loadUsage();
  };
  el("usageLoadMore").onclick = () => fetchPage({ append: true });
  el("usageDetailClose").onclick = () => closeDialog("usageDetailDialog");
}
