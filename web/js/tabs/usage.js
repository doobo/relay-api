/**
 * Usage tab: request log with server-side filters and incremental paging.
 *
 * The backend has always supported model/provider/kind/status/limit/offset;
 * this tab now uses them instead of always showing the newest 50 rows.
 */

import { api } from "../core/api.js";
import { closeDialog, el, fillTable, fillTableMessage, monoCell, openDialog, statusPill } from "../core/dom.js";
import { fmtTime } from "../core/format.js";
import { notifyError } from "../core/notice.js";

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

function queryString() {
  const params = new URLSearchParams();
  for (const [name, id] of Object.entries(FILTERS)) {
    const value = el(id).value.trim();
    if (value) params.set(name, value);
  }
  params.set("limit", String(PAGE_SIZE));
  params.set("offset", String(offset));
  return params.toString();
}

function render() {
  fillTable(
    "usageTable",
    rows.map((u) => [
      fmtTime(u.created_at),
      u.request_id,
      u.kind,
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

/** Called on tab entry: reload from the first page with the current filters. */
export async function loadUsage() {
  offset = 0;
  await fetchPage({ append: false });
}

export function initUsage() {
  el("usageFilters").onsubmit = (event) => {
    event.preventDefault();
    loadUsage();
  };
  el("usageFilterReset").onclick = () => {
    for (const id of Object.values(FILTERS)) el(id).value = "";
    loadUsage();
  };
  el("usageLoadMore").onclick = () => fetchPage({ append: true });
  el("usageDetailClose").onclick = () => closeDialog("usageDetailDialog");
}
