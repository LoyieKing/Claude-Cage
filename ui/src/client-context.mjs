let current = Object.freeze({ mode: "none", connection_id: "" });

export function setClientContext(value) {
  current = Object.freeze({ ...value });
}

export const getClientContext = () => current;
export const isRemoteClient = () => current.mode === "remote";

export function assertConnection(connectionID) {
  if (current.connection_id !== connectionID) {
    const error = new Error("工作环境已切换，请在当前环境重新操作。");
    error.code = "connection_changed";
    throw error;
  }
}

export function connectionHeaders(path, context = current) {
  if (path === "/session" || (path.startsWith("/client/") && !isProjectSharesPath(path))) return {};
  return { "X-Cage-Connection": context.connection_id || "disconnected" };
}

export const isProjectSharesPath = (path) => path === "/client/project-shares" || path.startsWith("/client/project-shares/");

export function runtimeURL(path, context = current) {
  const url = new URL("/api" + path, globalThis.location?.origin || "http://localhost");
  // Desktop keeps the view generation here. A retired SSH connection id must not be sent.
  url.searchParams.set("connection", context.connection_id || "disconnected");
  return url.pathname + url.search;
}

export function runtimeSocketURL(path, context = current) {
  return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${runtimeURL(path, context)}`;
}

export function clientStorageKey(key, context = current) {
  return context.mode === "remote"
    ? `${key}:remote:${context.instance_id || context.host_id || "disconnected"}`
    : key;
}

export function clientKeyboard(serverKeyboard, context = current) {
  if (context.mode !== "remote") return serverKeyboard;
  try {
    const saved = localStorage.getItem(clientStorageKey("cage-keyboard", context));
    if (["mac", "native"].includes(saved)) return saved;
  } catch { /* The client platform remains a usable default without storage. */ }
  return context.client_platform === "darwin" ? "mac" : "native";
}

export function saveClientKeyboard(value) {
  if (!["mac", "native"].includes(value)) throw new Error("键盘习惯无效");
  localStorage.setItem(clientStorageKey("cage-keyboard"), value);
  window.dispatchEvent(new Event("cage-client-preferences"));
}

export function transportGeneration(context = current) {
  if (context?.mode === "remote") return context.connection?.connection_id || context.connection_id || "";
  return context?.connection_id || "";
}

// A new connection id is a new generation, even for the same instance. Reload
// rather than letting an in-flight result continue on the replacement transport.
export function recoveryDecision(previous, next) {
  const keep = { reload: false, reattachReadonly: false, dropPending: false };
  const reload = { reload: true, reattachReadonly: false, dropPending: true };
  if (!previous?.mode || !next) return keep;
  if (previous.mode !== next.mode || Boolean(previous.desktop_gateway) !== Boolean(next.desktop_gateway)) return reload;
  if ((previous.connection_id || "") !== (next.connection_id || "")) return reload;
  if ((previous.instance_id || "") !== (next.instance_id || "")) return reload;
  return keep;
}
