/** Helpers for the form controls shared across tabs. */

export const COMMON_METHODS = ["POST", "GET", "PUT", "DELETE", "PATCH"];

/**
 * Value that pins no method: the gateway then forwards whatever verb the
 * client used, so one API config answers GET, POST, … alike. Must match
 * METHOD_NONE in src/utils/http.ts.
 */
export const METHOD_NONE = "NONE";

/** Values the <select> offers directly; anything else becomes "Custom…". */
const SELECT_METHODS = [...COMMON_METHODS, METHOD_NONE];

/**
 * Wire a method <select> + "Custom…" input pair. Selecting Custom… reveals the
 * free-text input; picking any listed method (common verbs and None) hides and
 * clears it.
 */
export function bindMethodSelect(selectEl, customInput) {
  selectEl.addEventListener("change", () => {
    const isCustom = selectEl.value === "";
    customInput.classList.toggle("hidden", !isCustom);
    if (isCustom) customInput.focus();
    else customInput.value = "";
  });
}

/** Shows the current method: a listed one directly, anything else as Custom…. */
export function setMethodValue(selectEl, customInput, method) {
  const value = (method || "POST").toUpperCase();
  if (SELECT_METHODS.includes(value)) {
    selectEl.value = value;
    customInput.classList.add("hidden");
    customInput.value = "";
  } else {
    selectEl.value = "";
    customInput.classList.remove("hidden");
    customInput.value = value;
  }
}

/** Resolves the select + optional custom input into the final method string. */
export function getMethodValue(selectEl, customInput) {
  if (selectEl.value === "") {
    return (customInput?.value || "").trim().toUpperCase();
  }
  return selectEl.value;
}

/** Parse an optional JSON form field: blank -> undefined (leave unchanged). */
export function parseJsonInput(raw, label) {
  const value = (raw || "").trim();
  if (!value) return undefined;
  try {
    return JSON.parse(value);
  } catch {
    throw new Error(`${label} is not valid JSON`);
  }
}

export function safePrettyJson(raw) {
  if (!raw) return "";
  try {
    return JSON.stringify(JSON.parse(raw));
  } catch {
    return String(raw);
  }
}
