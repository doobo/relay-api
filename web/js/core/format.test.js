/**
 * Unit tests for the shared formatters (bun:test, no DOM).
 *
 * Expectations are built from local Date parts, so they hold in any timezone -
 * fmtTime deliberately renders local time, matching what the server stored.
 */

import { expect, test } from "bun:test";
import { fmtNumber, fmtTime } from "./format.js";

test("fmtTime renders the full local date and time", () => {
  expect(fmtTime(new Date(2026, 8, 20, 9, 5, 7).getTime())).toBe("2026-09-20 09:05:07");
});

test("fmtTime zero-pads every part", () => {
  expect(fmtTime(new Date(2026, 0, 1, 0, 0, 0).getTime())).toBe("2026-01-01 00:00:00");
});

test("fmtTime keeps late-evening times on their own day", () => {
  expect(fmtTime(new Date(2026, 11, 31, 23, 59, 59).getTime())).toBe("2026-12-31 23:59:59");
});

test("fmtNumber abbreviates thousands and millions", () => {
  expect(fmtNumber(42)).toBe("42");
  expect(fmtNumber(1500)).toBe("1.5K");
  expect(fmtNumber(2_300_000)).toBe("2.3M");
});
