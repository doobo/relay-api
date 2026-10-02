/** Time and number formatting helpers. */

/**
 * Epoch ms -> "2026-09-20 09:11:27" in the browser's local time.
 *
 * Built from the Date parts instead of toLocaleString(): these values fill data
 * tables, so a column should read the same in every browser locale, and the rows
 * span days - a bare clock time (what toLocaleTimeString gave) can't be placed
 * on a calendar. Used by the Logs / Usage / Recent Requests "Time" columns and
 * by the Keys "Last Used", Settings "Last Login" / "Created" cells.
 */
export function fmtTime(ts) {
  const date = new Date(ts);
  const pad = (value) => String(value).padStart(2, "0");
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

export function fmtNumber(n) {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return String(n);
}
