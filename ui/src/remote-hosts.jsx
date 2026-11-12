import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  ArrowLeft,
  Check,
  Computer,
  LoaderCircle,
  Plus,
  Server,
  ShieldCheck,
} from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { recoveryDecision, setClientContext } from "./client-context.mjs";
import { Modal } from "./modal";
import { HostForm } from "./remote-host-form";
import logo from "./logo.png";
import "./remote-hosts.css";

const Navigation = createContext(null);
export const useSSHNavigation = () => useContext(Navigation);
const occupied = (status) =>
  Boolean(status?.running || status?.busy || status?.live_terminals);
const unknown = () => ({ known: false, message: "本机状态待确认" });
const recentKey = "cage-recent-environment";
function recent() {
  try {
    return localStorage.getItem(recentKey) || "";
  } catch {
    return "";
  }
}
function remember(value) {
  try {
    localStorage.setItem(recentKey, value);
  } catch {
    /* Optional preference. */
  }
}
function localSummary(status) {
  if (!status?.known) return "本机状态待确认";
  if (status.busy) return "本机正在处理环境操作";
  if (status.live_terminals)
    return `本机有 ${status.live_terminals} 个活动终端`;
  if (status.running) return "本机环境正在运行";
  return status.service_running ? "本机控制服务已启动" : "本机环境已停止";
}

export function ClientRoot({ children }) {
  const [context, setContext] = useState(null);
  const [error, setError] = useState("");
  const previousContext = useRef(null);
  const mounted = useRef(false);
  const serial = useRef(0);
  const refresh = useCallback(async () => {
    const query = ++serial.current;
    const value = await api("/client/context");
    if (!mounted.current || query !== serial.current) return value;
    if (!["none", "local", "remote"].includes(value.mode))
      throw new Error("客户端未返回有效的工作环境，请更新 Cage。");
    const decision = recoveryDecision(previousContext.current, value);
    previousContext.current = value;
    setClientContext(value);
    if (decision.reload) {
      // Destroy callbacks, local files in flight and input handlers from the previous generation.
      location.reload();
      return value;
    }
    setContext(value);
    setError("");
    return value;
  }, []);
  useEffect(() => {
    mounted.current = true;
    let timer;
    async function poll() {
      try {
        await refresh();
      } catch (e) {
        if (mounted.current) setError(e.message);
      }
      if (mounted.current) timer = setTimeout(poll, 2000);
    }
    (async () => {
      if (location.hash) {
        await api("/session", { token: location.hash.slice(1) });
        history.replaceState(null, "", location.pathname);
      }
      window.dispatchEvent(new Event("cage-session-ready"));
      await poll();
    })().catch((e) => setError(e.message));
    return () => {
      mounted.current = false;
      clearTimeout(timer);
    };
  }, [refresh]);
  return (
    <div className="client-shell">
      {error && (
        <div className="client-error error-message" role="alert">
          {error}
          <button onClick={() => location.reload()}>重新连接</button>
        </div>
      )}
      {context ? (
        <Environment context={context} refresh={refresh}>
          {children}
        </Environment>
      ) : (
        !error && (
          <div className="client-loading">
            <LoaderCircle className="spin" size={18} />
            正在连接 Cage…
          </div>
        )
      )}
    </div>
  );
}

function Environment({ context, refresh, children }) {
  const gateway = Boolean(context.desktop_gateway);
  const local = context.mode === "local" && !context.client_only;
  const remote = context.mode === "remote";
  const liveConnection = context.connection;
  const reconnecting = remote && liveConnection?.state === "reconnecting";
  const active = remote && liveConnection?.state === "connected";
  const localLost =
    gateway && local && context.connection?.state !== "connected";
  const ready = (local && !localLost) || active || reconnecting;
  const [expanded, setExpanded] = useState(false);
  const [hosts, setHosts] = useState([]);
  const [selected, setSelected] = useState(context.host_id || recent());
  const [form, setForm] = useState(null);
  const [remove, setRemove] = useState(null);
  const [stop, setStop] = useState(null);
  const [decision, setDecision] = useState(null);
  const [localLocked, setLocalLocked] = useState(false);
  const [localStatus, setLocalStatus] = useState(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [password, setPassword] = useState("");
  const runningAction = useRef(false);
  const serial = useRef(0);
  const secretForTrust = useRef(null);
  const shellPresent = useRef(true);
  const acknowledged = useRef(false);
  const awaitingTarget =
    !ready && gateway && !context.client_only && context.mode === "none";
  const [stayInSSH, setStayInSSH] = useState(() =>
    sessionStorage.getItem("cage-disconnected-generation") === context.connection_id
  );
  const defaultLocal = awaitingTarget && !stayInSSH && context.candidate?.mode !== "remote";
  const showHosts = expanded || (!ready && !defaultLocal);
  const candidate = gateway ? context.candidate : null;
  const verifying = candidate?.connection?.state === "connecting";
  const connection =
    candidate?.connection ||
    (!gateway || liveConnection?.state !== "connected" ? liveConnection : null);
  const host = hosts.find((item) => item.id === selected);
  const hostConnected = Boolean((active || reconnecting) && host?.id === context.host_id);
  const connectionHostID = connection?.host_id || candidate?.host_id;
  const pinnedFailure =
    connectionHostID === selected &&
    connection?.host_key &&
    ["host_key_unknown", "host_key_changed"].includes(connection.error_code);
  const needsDeploy = connectionHostID === selected && connection?.error_code === "deployment_required";
  const phase = connection?.phase || "";
  const connectingLabel = phase.startsWith("cloud_upload_")
    ? `正在上传部署包 ${phase.slice("cloud_upload_".length)}%`
    : { cloud_install: "正在安装云端环境…", cloud_detect: "正在检测服务器…", cloud_start: "正在启动服务…" }[phase] || "正在验证…";
  const hostName = remote
    ? context.name || "远程主机"
    : local
      ? "本机环境"
      : "选择工作环境";
  const route = () => ({ connection_id: context.connection_id || "" });
  const load = useCallback(async () => {
    const { hosts = [] } = await api("/client/hosts");
    setHosts(hosts);
    setSelected((id) =>
      hosts.some((item) => item.id === id) ? id : hosts[0]?.id || "",
    );
  }, []);
  const statusSerial = useRef(0);
  const readLocal = useCallback(async () => {
    const id = ++statusSerial.current;
    let result;
    try {
      result = await api("/client/local-status");
    } catch {
      result = unknown();
    }
    if (shellPresent.current && id === statusSerial.current)
      setLocalStatus(result);
    return result;
  }, []);
  useEffect(() => {
    shellPresent.current = true;
    load().catch((e) => setError(e.message));
    return () => {
      shellPresent.current = false;
      ++serial.current;
    };
  }, [load]);
  useEffect(() => {
    if (!gateway || (local && !showHosts && !decision)) return;
    let live = true,
      timer;
    async function poll() {
      if (document.visibilityState !== "hidden") await readLocal();
      if (live) timer = setTimeout(poll, 4500);
    }
    poll();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [gateway, local, showHosts, decision, readLocal]);
  useEffect(() => {
    if (!ready && !defaultLocal && !acknowledged.current && nativeAvailable()) {
      native("workbenchReady")
        .then((ok) => {
          if (ok === true) acknowledged.current = true;
        })
        .catch(() => {});
    }
  }, [ready, defaultLocal]);
  const act = async (name, operation) => {
    if (runningAction.current) return;
    runningAction.current = true;
    setBusy(name);
    setError("");
    setLocalLocked(false);
    try {
      await operation();
    } catch (e) {
      setLocalLocked(e.status === 423);
      setError(
        e.status === 423
          ? "本机环境已锁定。请先返回本机解锁，再停止本机环境。"
          : e.message,
      );
    } finally {
      runningAction.current = false;
      setBusy("");
    }
  };
  const closeSidebar = () => {
    secretForTrust.current = null;
    setExpanded(false);
    setPassword("");
    setError("");
  };
  const openSidebar = () => {
    setExpanded(true);
    setError("");
  };
  const cancel = async (pending = candidate) => {
    ++serial.current;
    if (pending) {
      try {
        await api("/client/cancel", { ...route(), candidate_id: pending.id });
      } catch (e) {
        // Another window may already have cancelled this candidate. Never
        // dismiss a newer one, and keep retry available for a real failure.
        if (e.status !== 409 || (await refresh()).candidate?.id === pending.id)
          throw e;
      }
    }
    secretForTrust.current = null;
    setDecision(null);
    setPassword("");
    await refresh();
  };
  async function commit(pending, action, status) {
    try {
      await api("/client/commit", {
        ...route(),
        candidate_id: pending.id,
        local_action: action,
        ...(action === "stop"
          ? { local_controller_id: status?.controller_id }
          : {}),
      });
    } catch (e) {
      if (action === "auto" && e.status === 409 && pending.mode === "remote") {
        const result = await readLocal();
        if (occupied(result) || !result.known) {
          setDecision({ pending, status: result });
          return;
        }
      }
      if (action === "stop" && e.status !== 423) {
        const actual = await readLocal();
        if (actual.known && !actual.service_running && !occupied(actual)) {
          setDecision(null);
          throw new Error(
            "本机环境已停止。目标连接未完成，请重新连接 SSH 主机。",
          );
        }
      }
      throw e;
    }
    remember(
      pending.mode === "local"
        ? "local"
        : pending.connection?.host_id || pending.host_id || selected,
    );
    setDecision(null);
    setPassword("");
    await refresh();
  }
  async function verified(pending) {
    if (pending.mode === "remote") {
      const status = await readLocal();
      if (!status.known || occupied(status)) {
        setDecision({ pending, status });
        return;
      }
    }
    await commit(pending, "auto");
  }
  // Advance only the candidate this window requested. An interrupted window can
  // explicitly resume a verified candidate without applying another window's choice.
  async function requestTarget(
    mode,
    item = null,
    secret = "",
    cancelled = false,
    deploy = false,
  ) {
    setPassword("");
    if (!gateway) {
      const old = context.connection;
      if (mode === "local")
        await api("/client/local", { connection_id: old?.connection_id || "" });
      else {
        if (old?.connection_id)
          await api("/client/disconnect", { connection_id: old.connection_id });
        await api("/client/connect", { host_id: item.id, passphrase: secret, deploy });
      }
      await refresh();
      return;
    }
    if (candidate && !cancelled) await cancel(candidate);
    secretForTrust.current = null;
    const pending = await api("/client/candidate", {
      ...route(),
      mode,
      ...(item ? { host_id: item.id, passphrase: secret, deploy } : {}),
    });
    const request = ++serial.current;

    let progress = pending;
    while (shellPresent.current && serial.current === request) {
      if (progress.connection?.state === "connected") {
        await verified(progress);
        return;
      }
      if (
        progress.connection?.state === "failed" ||
        progress.connection?.state === "needs_auth" ||
        progress.connection?.state === "disconnected"
      ) {
        if (item && secret && (progress.connection?.host_key || progress.connection?.error_code === "deployment_required"))
          secretForTrust.current = { id: item.id, secret };
        await refresh();
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, 350));
      if (!shellPresent.current || request !== serial.current) return;
      const value = await refresh();
      if (
        value.connection_id !== context.connection_id ||
        value.candidate?.id !== pending.id
      )
        return;
      progress = value.candidate;
    }
  }
  const trust = async () => {
    const saved = hosts.find((h) => h.id === connectionHostID);
    if (!saved || !connection.host_key)
      throw new Error("主机配置已变化，请重新连接。");
    const secret =
      password ||
      (secretForTrust.current?.id === saved.id
        ? secretForTrust.current.secret
        : "");
    if (candidate) await cancel(candidate);
    await api(
      `/client/hosts/${saved.id}`,
      { ...saved, host_key: connection.host_key },
      "PUT",
    );
    await load();
    setSelected(saved.id);
    await requestTarget("remote", saved, secret, true);
  };
  const disconnect = async () => {
    const next = await api(
      "/client/disconnect",
      gateway
        ? route()
        : { connection_id: context.connection?.connection_id || "" },
    );
    remember(context.host_id || selected);
    setPassword("");
    setExpanded(true);
    if (gateway) {
      sessionStorage.setItem("cage-disconnected-generation", next.connection_id);
      setStayInSSH(true);
    }
    await refresh();
  };
  const choose = (item) => {
    secretForTrust.current = null;
    setSelected(item.id);
    setPassword("");
    setError("");
    openSidebar();
  };
  // Enter the ordinary local account/workbench on a fresh desktop launch.
  // Connecting this control service never invokes /start (the VM stays manual).
  const attemptedDefault = useRef(false);
  useEffect(() => {
    if (!defaultLocal || attemptedDefault.current) return;
    attemptedDefault.current = true;
    void act("local", () => requestTarget("local"));
  }, [defaultLocal]);
  const localButton = (label) => (
    <button
      disabled={
        Boolean(busy) ||
        (localStatus?.available === false &&
          localStatus?.known &&
          !localStatus.service_running)
      }
      onClick={() => act("local", () => requestTarget("local"))}
    >
      <Computer size={16} />
      {label}
    </button>
  );
  const modalOpen = Boolean(form || remove || stop || decision);
  const decisionBusy = busy === "commit" || busy === "cancel";
  const cancelDecision = () => act("cancel", () => cancel(decision.pending));
  const unlockButton = localLocked && (
    <button
      disabled={Boolean(busy)}
      onClick={() =>
        act("unlock-local", async () => {
          const pending = decision?.pending || candidate;
          if (pending) await cancel(pending);
          setStop(null);
          setDecision(null);
          if (local && !localLost) {
            location.reload();
            return;
          }
          setExpanded(true);
          await requestTarget("local", null, "", true);
        })
      }
    >
      返回本机解锁
    </button>
  );
  const access = { expanded: showHosts, open: openSidebar };
  const showBackground =
    gateway &&
    !local &&
    localStatus &&
    (!localStatus.known ||
      occupied(localStatus) ||
      localStatus.service_running);
  return (
    <Navigation.Provider value={access}>
      {!local && !defaultLocal && <header className="client-toolbar">
        {local ? (
          <Computer size={16} />
        ) : remote ? (
          <Server size={16} />
        ) : (
          <img className="brand-logo" src={logo} alt="" />
        )}
        <strong>{ready ? hostName : "Cage"}</strong>
        {ready && (
          <span className="client-toolbar-hint">
            {reconnecting ? "正在重新连接 · 未完成的操作不会重试" : remote ? "SSH · 云端运行" : "当前工作环境"}
          </span>
        )}
        {ready && !showHosts && (
          <button className="ssh-fallback" onClick={openSidebar}>
            <Server size={15} />
            SSH 主机
          </button>
        )}
      </header>}
      {showBackground && (
        <div className="local-background" role="status">
          <Computer size={16} />
          <div>
            <strong>
              {occupied(localStatus)
                ? "本机也在运行"
                : localSummary(localStatus)}
            </strong>
            {occupied(localStatus) && (
              <span>
                {localStatus.live_terminals
                  ? `${localStatus.live_terminals} 个活动终端`
                  : localStatus.busy
                    ? "有操作正在进行"
                    : "正在占用本机资源"}
              </span>
            )}
          </div>
          {localStatus.known && localStatus.controller_id && (
            <button
              disabled={Boolean(busy)}
              onClick={() => {
                setError("");
                setStop(localStatus);
              }}
            >
              停止本机环境
            </button>
          )}
        </div>
      )}
      <div
        className="client-stage"
        inert={modalOpen || undefined}
        data-environment-dialog={modalOpen || undefined}
      >
        {showHosts && (
          <aside
            className="workspace-sidebar ssh-sidebar"
            aria-label="SSH 主机"
          >
            <div className="sidebar-top">
              <span className="sidebar-brand">
                <Server size={18} />
                SSH 主机
              </span>
              {ready && (
                <button
                  className="icon-button"
                  disabled={Boolean(busy) || verifying}
                  onClick={closeSidebar}
                  aria-label="返回工作台侧栏"
                >
                  <ArrowLeft size={16} />
                </button>
              )}
            </div>
            <div className="ssh-sidebar-body">
              <div className="ssh-list-heading">
                <span>已保存的主机</span>
                <button
                  className="icon-button"
                  aria-label="添加远程主机"
                  title="添加远程主机"
                  disabled={Boolean(busy)}
                  onClick={() => setForm({ host: null })}
                >
                  <Plus size={16} />
                </button>
              </div>
              {hosts.map((item) => (
                <button
                  className={`ssh-host-row ${selected === item.id ? "selected" : ""}`}
                  key={item.id}
                  aria-pressed={selected === item.id}
                  disabled={Boolean(busy) || verifying}
                  onClick={() => choose(item)}
                >
                  <Server size={15} />
                  <span>
                    <strong>{item.name}</strong>
                    <small>
                      {item.user}@{item.host}:{item.port}
                    </small>
                  </span>
                  {context.host_id === item.id && (active || reconnecting) && (
                    <Check size={15} aria-label={reconnecting ? "正在重新连接" : "已连接"} />
                  )}
                </button>
              ))}
              {!hosts.length && (
                <div className="ssh-empty">
                  <p>还没有远程主机</p>
                  <button onClick={() => setForm({ host: null })}>
                    <Plus size={14} />
                    添加主机
                  </button>
                </div>
              )}
              {host && (
                <section className="ssh-host-actions" aria-label="所选主机操作">
                  {host.auth.kind === "key_file" && !hostConnected && (
                    <label>
                      密钥口令（如果已设置）
                      <input
                        type="password"
                        autoComplete="off"
                        disabled={Boolean(busy)}
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                      />
                    </label>
                  )}
                  {hostConnected ? (
                    <button
                      disabled={Boolean(busy)}
                      onClick={() => act("disconnect", disconnect)}
                    >
                      断开连接
                    </button>
                  ) : (
                    !pinnedFailure && (
                      <button
                        className="primary"
                        disabled={
                          Boolean(busy) || verifying || Boolean(decision)
                        }
                        onClick={() =>
                          act("verify", () =>
                            requestTarget("remote", host, password || (needsDeploy && secretForTrust.current?.id === host.id ? secretForTrust.current.secret : ""), false, needsDeploy),
                          )
                        }
                      >
                        {busy === "verify" || verifying
                          ? connectingLabel
                          : needsDeploy ? "部署并连接" : "连接此主机"}
                      </button>
                    )
                  )}
                  <div className="remote-button-row">
                    <button
                      disabled={Boolean(busy) || hostConnected || verifying}
                      onClick={() =>
                        act("edit", async () => {
                          if (candidate?.connection.host_id === host.id)
                            await cancel(candidate);
                          setForm({ host });
                        })
                      }
                    >
                      编辑
                    </button>
                    <button
                      disabled={Boolean(busy) || hostConnected || verifying}
                      onClick={() =>
                        act("remove-prompt", async () => {
                          if (candidate?.connection.host_id === host.id)
                            await cancel(candidate);
                          setRemove(host);
                        })
                      }
                    >
                      移除连接
                    </button>
                  </div>
                </section>
              )}
              {!context.client_only && (!local || localLost) && (
                <div className="ssh-local-action">
                  {localButton("进入本机工作台")}
                </div>
              )}
              {!context.client_only && local && (
                <p className="ssh-help">云端连接就绪后再切换工作台。</p>
              )}
              {error && !decision && !stop && !remove && (
                <p className="error-message" role="alert">
                  {error}
                </p>
              )}
              {["failed", "needs_auth"].includes(connection?.state) && !reconnecting &&
                (!connectionHostID || connectionHostID === selected) && (
                  <div className="ssh-connection" role="alert">
                    <strong>
                      {connection.message || "连接失败，请重试。"}
                    </strong>
                    {pinnedFailure && (
                      <>
                        <p>请与服务器管理员核对“{host?.name}”的主机指纹。</p>
                        <code>
                          {connection.host_key.algorithm}
                          <br />
                          {connection.host_key.fingerprint}
                        </code>
                        <button
                          disabled={Boolean(busy)}
                          className="primary"
                          onClick={() => act("trust", trust)}
                        >
                          <ShieldCheck size={15} />
                          我已核对指纹，信任此主机
                        </button>
                      </>
                    )}
                  </div>
                )}
              {remote && !active && !reconnecting && !connection?.message && (
                <p className="ssh-help" role="status">
                  连接已断开。请重新连接并核对云端任务。
                </p>
              )}
              {gateway && candidate && !decision && (
                <>
                  {candidate.connection?.state === "connected" && !busy && (
                    <button
                      onClick={() => act("verify", () => verified(candidate))}
                    >
                      继续连接
                    </button>
                  )}
                  <button
                    disabled={decisionBusy}
                    onClick={() => cancel().catch((e) => setError(e.message))}
                  >
                    取消此次连接
                  </button>
                </>
              )}
            </div>
          </aside>
        )}
        {ready ? (
          <div className="client-workbench">{children}</div>
        ) : defaultLocal && !error && candidate?.connection?.state !== "failed" && !expanded ? (
          <div className="client-loading" role="status">正在打开 Cage 工作台…</div>
        ) : (
          <main className="environment-picker">
            <div>
              <img className="environment-logo" src={logo} alt="" />
              <h1>
                {localLost
                  ? "重新连接本机工作台"
                  : remote && !active
                    ? "重新连接 SSH 主机"
                    : defaultLocal ? "本机工作台连接未完成" : "连接 SSH 主机"}
              </h1>
              <p>
                {context.client_only
                  ? "通过 SSH 连接服务器，首次连接可自动部署 Cage。项目、应用和网络由服务器管理。"
                  : "本机和 SSH 主机使用同一个工作台。项目、应用和网络由选中的环境管理。"}
              </p>
              {!context.client_only &&
                localStatus?.known &&
                localStatus.available === false &&
                !localStatus.service_running && (
                  <p>本机运行组件不可用，可通过 SSH 连接服务器。</p>
                )}
              {!showHosts && error && (
                <p className="error-message" role="alert">
                  {error}
                </p>
              )}
              <div className="environment-actions">
                {!context.client_only &&
                  (!showHosts || localLost) &&
                  localButton("重新连接本机")}
                {!showHosts && (
                  <button disabled={Boolean(busy)} onClick={openSidebar}>
                    <Server size={16} />
                    连接 SSH 主机
                  </button>
                )}
              </div>
              {!showHosts && hosts.length > 0 && (
                <div className="environment-recent">
                  <h2>已保存的主机</h2>
                  {hosts.map((item) => (
                    <button key={item.id} onClick={() => choose(item)}>
                      <Server size={15} />
                      {item.name}
                      {recent() === item.id && <small>上次使用</small>}
                    </button>
                  ))}
                </div>
              )}
              {busy === "local" && <p role="status">正在连接本机工作台…</p>}
              {!showHosts && candidate?.connection?.state === "failed" && (
                <p role="alert" className="error-message">
                  {candidate.connection.message}
                </p>
              )}
            </div>
          </main>
        )}
      </div>
      {form && (
        <HostForm
          initial={form.host}
          platform={context.client_platform}
          onClose={() => setForm(null)}
          onSaved={async (saved) => {
            await load();
            setSelected(saved.id);
            setPassword("");
            setForm(null);
          }}
        />
      )}
      {remove && (
        <Modal
          title="移除 SSH 主机"
          onClose={busy ? undefined : () => setRemove(null)}
          footer={
            <>
              <button disabled={Boolean(busy)} onClick={() => setRemove(null)}>
                取消
              </button>
              <button
                className="primary"
                disabled={Boolean(busy)}
                onClick={() =>
                  act("remove", async () => {
                    await api(
                      `/client/hosts/${remove.id}`,
                      undefined,
                      "DELETE",
                    );
                    setRemove(null);
                    setPassword("");
                    await load();
                  })
                }
              >
                确认移除
              </button>
            </>
          }
        >
          <div className="ssh-dialog-body">
            <p>移除“{remove.name}”的本机连接配置？云端文件和任务会保留。</p>
            {error && <p role="alert">{error}</p>}
          </div>
        </Modal>
      )}
      {decision && (
        <Modal
          title="切换到 SSH 主机"
          onClose={decisionBusy ? undefined : cancelDecision}
          footer={
            <>
              <button disabled={decisionBusy} onClick={cancelDecision}>
                取消
              </button>
              <button
                disabled={decisionBusy}
                onClick={() =>
                  act("commit", () =>
                    commit(decision.pending, "keep", decision.status),
                  )
                }
              >
                保留本机运行
              </button>
              <button
                className="primary"
                disabled={
                  decisionBusy ||
                  !decision.status.known ||
                  !decision.status.controller_id
                }
                onClick={() =>
                  act("commit", () =>
                    commit(decision.pending, "stop", decision.status),
                  )
                }
              >
                停止本机并连接
              </button>
            </>
          }
        >
          <div className="ssh-dialog-body">
            <p>
              <Check size={16} /> 已验证{" "}
              {hosts.find(
                (h) =>
                  h.id ===
                  (decision.pending.connection?.host_id ||
                    decision.pending.host_id),
              )?.name || "目标主机"}{" "}
              的 SSH 与 Cage 服务。
            </p>
            <p>
              <strong>{localSummary(decision.status)}。</strong>{" "}
              停止本机会结束终端任务并释放资源，未保存的内容可能丢失。选择保留后可以在云端工作台稍后停止。
            </p>
            {busy === "commit" && (
              <p role="status">正在确认环境切换，请稍候…</p>
            )}
            {error && (
              <p role="alert" className="error-message">
                {error}
              </p>
            )}
            {unlockButton}
          </div>
        </Modal>
      )}
      {stop && (
        <Modal
          title="停止本机环境？"
          onClose={busy ? undefined : () => setStop(null)}
          footer={
            <>
              <button disabled={Boolean(busy)} onClick={() => setStop(null)}>
                取消
              </button>
              <button
                className="primary"
                disabled={Boolean(busy)}
                onClick={() =>
                  act("stop", async () => {
                    await api("/client/local-stop", {
                      ...route(),
                      local_controller_id: stop.controller_id,
                    });
                    await readLocal();
                    setStop(null);
                  })
                }
              >
                确认停止本机
              </button>
            </>
          }
        >
          <div className="ssh-dialog-body">
            <p>
              {localSummary(stop)}
              。停止将结束本机终端任务并释放资源，未保存的内容可能丢失。云端任务继续运行。
            </p>
            {busy === "stop" && <p role="status">正在停止本机环境…</p>}
            {error && (
              <p role="alert" className="error-message">
                {error}
              </p>
            )}
            {unlockButton}
          </div>
        </Modal>
      )}
    </Navigation.Provider>
  );
}
