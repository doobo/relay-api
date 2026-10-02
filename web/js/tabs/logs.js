/** Audit logs tab (token reveals). */

import { api } from "../core/api.js";
import { fillTable, fillTableMessage } from "../core/dom.js";
import { fmtTime } from "../core/format.js";

export async function loadLogs() {
  try {
    const data = await api("/admin/logs");
    fillTable(
      "logsTable",
      data.data.map((log) => [fmtTime(log.created_at), log.action, log.target, log.source_ip]),
    );
  } catch (e) {
    fillTableMessage("logsTable", `Failed to load: ${e.message}`, { error: true });
  }
}
