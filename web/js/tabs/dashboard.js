/** Dashboard tab: today's counters, per-model stats and recent requests. */

import { api } from "../core/api.js";
import { el, fillTable, fillTableMessage, monoCell, statusPill } from "../core/dom.js";
import { fmtNumber, fmtTime } from "../core/format.js";

export async function loadDashboard() {
  try {
    const stats = await api("/admin/stats");
    el("statRequests").textContent = fmtNumber(stats.today.requests);
    el("statTokens").textContent = fmtNumber(stats.today.tokens);
    el("statErrors").textContent = fmtNumber(stats.today.errors);
    el("statLatency").textContent = `${stats.today.avg_latency_ms}ms`;

    fillTable(
      "modelStatsTable",
      stats.models.map((m) => [m.model, m.requests, fmtNumber(m.tokens)]),
    );

    const usage = await api("/admin/usage?limit=10");
    fillTable(
      "recentTable",
      usage.data.map((u) => [
        fmtTime(u.created_at),
        u.kind,
        // For a forward the "target" is the upstream URL it went to.
        u.model || u.provider || (u.target_url ? monoCell(u.target_url) : u.api_config_id ? `config #${u.api_config_id}` : "-"),
        statusPill(u.status),
        `${u.latency_ms ?? "-"}ms`,
      ]),
    );
  } catch (e) {
    fillTableMessage("modelStatsTable", `Failed to load: ${e.message}`, { error: true });
  }
}
