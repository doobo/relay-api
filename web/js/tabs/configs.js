/** API configs tab: non-AI forwarding endpoints. */

import { api } from "../core/api.js";
import {
  askConfirm,
  bindInfoPopover,
  closeDialog,
  delButton,
  el,
  fillTable,
  fillTableMessage,
  openDialog,
  smallButton,
  wrapButtons,
} from "../core/dom.js";
import { clearNotice, notifyError } from "../core/notice.js";
import { bindMethodSelect, getMethodValue, parseJsonInput, safePrettyJson, setMethodValue } from "../core/forms.js";

/**
 * Render a config name with its mount and, for wildcard (path-pattern) names,
 * the kind of pattern, so both read at a glance in the list.
 */
function nameWithTag(cfg) {
  const name = cfg.name;
  const wrapper = document.createElement("span");
  wrapper.className = "config-name";

  const route = cfg.route === "free" ? "free" : "open";
  const routeTag = document.createElement("span");
  routeTag.className = `tag route-${route}`;
  routeTag.textContent = `/${route}`;
  routeTag.title =
    route === "free"
      ? "/free/* - public raw streaming tunnel (no API key, no templates)"
      : "/open/* - requires an API key";
  wrapper.append(routeTag);

  const segments = name.split("/");
  const subtree = segments.includes("**");
  const single = segments.includes("*");
  if (!subtree && !single) {
    wrapper.append(name);
    return wrapper;
  }

  const code = document.createElement("code");
  code.textContent = name;

  const tag = document.createElement("span");
  tag.className = "tag wildcard";
  if (subtree && single) {
    tag.textContent = "mixed";
    tag.title = "matches one segment (*) plus a whole subtree (**)";
  } else if (subtree) {
    tag.textContent = "subtree";
    tag.title = "matches every path below the prefix (api/**)";
  } else {
    tag.textContent = "1 segment";
    tag.title = "matches exactly one more path segment (api/*)";
  }

  wrapper.append(code, tag);
  return wrapper;
}

export async function loadConfigs() {
  try {
    const data = await api("/admin/api-configs");
    fillTable(
      "configsTable",
      data.data.map((cfg) => [
        cfg.id,
        nameWithTag(cfg),
        cfg.method,
        cfg.url,
        cfg.stats.requests,
        cfg.stats.errors,
        cfg.api_key_masked || "(none)",
        wrapButtons(
          smallButton("Edit", () => openConfigDialog("edit", cfg)),
          delButton(() =>
            askConfirm({
              title: `Delete config '${cfg.name}'?`,
              // The name may be a wildcard, so say where it was mounted rather
              // than listing the paths it happened to match.
              message: `Requests to /${cfg.route}/${cfg.name} stop being forwarded immediately. This cannot be undone.`,
              action: async () => {
                await api(`/admin/api-configs/${cfg.id}`, { method: "DELETE" });
                loadConfigs();
              },
            }),
          ),
        ),
      ]),
    );
  } catch (e) {
    fillTableMessage("configsTable", `Failed to load: ${e.message}`, { error: true });
  }
}

/**
 * Open the shared add/edit dialog. Creating and editing use the same fields -
 * the form only differs in its title/button label and in whether the upstream
 * key input is a rotation or a first-time value.
 */
function openConfigDialog(mode, cfg) {
  const form = el("configForm");
  form.reset();
  form.dataset.mode = mode;
  form.dataset.id = cfg ? String(cfg.id) : "";

  const title = el("configDialogTitle");
  if (mode === "edit" && cfg) title.replaceChildren("Edit config ", nameWithTag(cfg));
  else title.textContent = "Add config";
  el("configDialogSubmit").textContent = mode === "edit" ? "Save" : "Create";

  if (mode === "edit" && cfg) {
    form.elements.name.value = cfg.name;
    form.elements.url.value = cfg.url;
    form.elements.route.value = cfg.route === "free" ? "free" : "open";
    form.elements.streamTimeoutMs.value = cfg.stream_timeout_ms ? String(cfg.stream_timeout_ms) : "";
    form.elements.streamMaxBodyMb.value = cfg.stream_max_body_mb ? String(cfg.stream_max_body_mb) : "";
    setMethodValue(form.elements.method, form.elements.customMethod, cfg.method);
    form.elements.apiKey.value = "";
    form.elements.apiKey.placeholder = cfg.has_api_key
      ? "new upstream key (leave blank to keep current)"
      : "upstream key (optional)";
    form.elements.headers.value = safePrettyJson(cfg.headers);
    form.elements.requestTemplate.value = safePrettyJson(cfg.request_template);
    form.elements.responseTemplate.value = safePrettyJson(cfg.response_template);
    form.elements.enabled.checked = Boolean(cfg.enabled);
  } else {
    // form.reset() restores the markup defaults; only the method pair needs
    // normalising, because its custom input may still be revealed.
    setMethodValue(form.elements.method, form.elements.customMethod, "POST");
    form.elements.apiKey.placeholder = "upstream key (optional)";
    form.elements.enabled.checked = true;
  }

  openDialog("configDialog");
}

/** Parse a config's numeric limit input; blank or invalid means "unlimited" (0). */
function nonNegativeInt(raw) {
  const n = Number.parseInt(raw, 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

export function initConfigs() {
  bindMethodSelect(el("configMethod"), el("configCustomMethod"));
  bindInfoPopover("configInfoToggle", "configInfoPop");

  el("configCreateBtn").onclick = () => openConfigDialog("create");
  el("configDialogCancel").onclick = () => closeDialog("configDialog");

  el("configForm").onsubmit = async (event) => {
    event.preventDefault();
    const form = event.target;
    clearNotice(form);
    const mode = form.dataset.mode;
    const id = form.dataset.id;

    let headers;
    let requestTemplate;
    let responseTemplate;
    try {
      headers = parseJsonInput(form.elements.headers.value, "headers");
      requestTemplate = parseJsonInput(form.elements.requestTemplate.value, "requestTemplate");
      responseTemplate = parseJsonInput(form.elements.responseTemplate.value, "responseTemplate");
    } catch (e) {
      notifyError(e.message, { form });
      return;
    }

    const body = {
      name: form.elements.name.value,
      url: form.elements.url.value,
      method: getMethodValue(form.elements.method, form.elements.customMethod) || "POST",
      route: form.elements.route.value,
      // /free tunnel limits; 0 on both means unlimited. Ignored by /open.
      streamTimeoutMs: nonNegativeInt(form.elements.streamTimeoutMs.value),
      streamMaxBodyMb: nonNegativeInt(form.elements.streamMaxBodyMb.value),
      enabled: form.elements.enabled.checked,
    };
    // undefined = field unchanged (server keeps existing); null = cleared.
    if (headers !== undefined) body.headers = headers;
    if (requestTemplate !== undefined) body.requestTemplate = requestTemplate;
    if (responseTemplate !== undefined) body.responseTemplate = responseTemplate;
    const newKey = form.elements.apiKey.value.trim();
    if (newKey) body.apiKey = newKey; // only send when rotating

    try {
      if (mode === "edit") {
        await api(`/admin/api-configs/${id}`, { method: "PUT", body: JSON.stringify(body) });
      } else {
        await api("/admin/api-configs", { method: "POST", body: JSON.stringify(body) });
      }
      closeDialog("configDialog");
      loadConfigs();
    } catch (e) {
      notifyError(e.message, { form });
    }
  };
}
