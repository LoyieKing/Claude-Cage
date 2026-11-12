import { shortcut, primaryModifier } from "./platform.mjs";
import logo from "./logo.png";
import React, { useEffect, useRef, useState } from "react";
import {
  Clock3,
  FileCode2,
  FolderOpen,
  Globe2,
  LoaderCircle,
  MapPin,
  MoreHorizontal,
  Power,
  RefreshCw,
  Shield,
  TerminalSquare,
  X,
} from "lucide-react";
import { api, normalizeURL, native, nativeAvailable } from "./api";
import { clientKeyboard, clientStorageKey, getClientContext, isRemoteClient, runtimeURL, runtimeSocketURL, transportGeneration } from "./client-context.mjs";
import { shareClipboard } from "./shared-clipboard.mjs";
import { RemoteView, TerminalView, Modal } from "./views";
import { usePasswordVault } from "./password-vault";
import { SettingsPanel } from "./settings";
import { SetupProgress } from "./setup-progress";
import { GuestSystemPicker, validGuestChoice } from "./guest-system-picker";
import { AppearanceProvider } from "./appearance";
import { EnvironmentShield } from "./environment-shield";
import { TailscalePanel } from "./tailscale";
import { WorkspaceSidebar } from "./workspace-sidebar";
import { useCLISessions, sessionsChanged } from "./cli-sessions";
import { TerminalMenu } from "./terminal-menu";
import { InitializationGate } from "./initialization";
import { MacOSLogin } from "./macos-login";
import { MacOSClaudeWindow } from "./macos-claude-window";
import { ProjectRecovery } from "./project-recovery";
import { ClaudeDesktop } from "./claude-desktop";
import { useClaudeAccounts } from "./claude-accounts";
import { AccountProvider, AccountGate, useAccount } from "./account";
import { BrowserHeader, BrowserTabRail, TerminalHeader, ExpandSidebar, ExpandPane } from "./pane-headers";
import { BrowserDownloads, useDownloadNotifications, useDesktopFiles } from "./browser-downloads";
import { BrowserUploads } from "./browser-uploads";
import { BrowserSettingsPage } from "./browser-settings";
import { AudioControls, useAudioOutput } from "./audio";
import { ProjectDialog } from "./project-dialog";
import {
  readLocal,
  orderedTabs,
  focusTab,
  splitTab,
  projectName,
  tabName,
} from "./workspace-state.mjs";
import { SafetyPanel } from "./safety";
import { SafetyChecklistContext, SafetyMessage } from "./safety-message";
import { networkState, isGuestLocalURL } from "./safety-state.mjs";
import { needsProxySetup } from "./onboarding-state.mjs";
import { HostGitPreparation } from "./host-git";
import { NetworkLatency } from "./network-latency";
import { useWindowVisibility } from "./window-visibility";
import { AppUpdateButton, useAppUpdates } from "./app-updates";
import "./appearance.css";
import { stopAndExit } from "./quit";

function App() {
  const { account } = useAccount();
  const macGuest = account?.guest_system === "macos";
  const nativeGuestLabel = {macos: "macOS", "windows-11": "Windows"}[account?.guest_system];
  const [, refreshClientPreferences] = useState(0);
  useEffect(() => {
    const refresh = () => refreshClientPreferences((value) => value + 1);
    window.addEventListener("cage-client-preferences", refresh);
    return () => window.removeEventListener("cage-client-preferences", refresh);
  }, []);
  const claudeProfiles = useClaudeAccounts();
  const [tailscaleOpen, setTailscaleOpen] = useState(false);
  const [desktopOpen, setDesktopOpen] = useState(false);
  const [terminalChoice, setTerminalChoice] = useState(null);
  const [openingSession, setOpeningSession] = useState("");
  const [sessionOpenError, setSessionOpenError] = useState(null);
  const sessionOpenRequest = useRef(null);
  useEffect(() => () => {
    const request = sessionOpenRequest.current;
    sessionOpenRequest.current = null;
    request?.controller.abort();
  }, []);
  const terminalTrigger = useRef(null);
  const appUpdates = useAppUpdates();
  const [recoveringEditor, setRecoveringEditor] = useState(false);
  const windowVisible = useWindowVisibility();
  const windowVisibleRef = useRef(windowVisible);
  windowVisibleRef.current = windowVisible;
  const [serverStatus, setStatus] = useState(null),
    [settings, setSettings] = useState(null),
    [proxy, setProxy] = useState(null),
    [tabs, setTabs] = useState([]),
    [layout, setLayout] = useState(() => ({ active: localStorage.getItem(clientStorageKey("cage-active")) || "" })),
    [browser, setBrowser] = useState({ tabs: [] }),
    [browserSettingsRevision, setBrowserSettingsRevision] = useState(0),
    [settingsOpen, setSettingsOpen] = useState(false),
    [startOptions, setStartOptions] = useState(null),
    [guestSystem, setGuestSystem] = useState(undefined),
    [startRequest, setStartRequest] = useState(null),
    [settingsSection, setSettingsSection] = useState("keyboard"),
    [home, setHome] = useState(true),
    [sidebarCollapsed, setSidebarCollapsed] = useState(
      readLocal(clientStorageKey("cage-sidebar-collapsed"), false),
    ),
    [browserTabLayout, setBrowserTabLayout] = useState(
      readLocal(clientStorageKey("cage-browser-tab-layout"), "horizontal"),
    ),
    [selectedProject, setSelectedProject] = useState(""),
    [savedTabs, setSavedTabs] = useState(readLocal(clientStorageKey("cage-tab-layout"), {})),
    [privateTabMetadata, setPrivateTabMetadata] = useState({}),
    [projectDialog, setProjectDialog] = useState(null),
    [rename, setRename] = useState(null),
    [toast, setToast] = useState(""),
    [fatal, setFatal] = useState(""),
    [connectionError, setConnectionError] = useState(""),
    [browserError, setBrowserError] = useState(""),
    [dialog, setDialog] = useState(null),
    [transfer, setTransfer] = useState(null),
    [details, setDetails] = useState(false),
    [safetyOpen, setSafetyOpen] = useState(false),
    [quitting, setQuitting] = useState(null);
  // Keep feedback continuous until polling observes the accepted start job.
  const clientStatus = serverStatus && { ...serverStatus, keyboard: clientKeyboard(serverStatus.keyboard) };
  const hostGitOpen = startOptions !== null;
  const status = clientStatus && (hostGitOpen || startRequest)
    ? { ...clientStatus, job: { running: true, kind: "start", message: hostGitOpen
      ? isRemoteClient() ? "正在检查云端主机 Git 安全保护…" : "正在检查本机 Git 安全保护…" : "正在启动 Cage 环境…" } }
    : clientStatus;
  const { active } = layout;
  const setActive = id => setLayout(previous => focusTab(previous, id));
  const workspaceBox = useRef(null);
  const [dropSide, setDropSide] = useState(null);
  const terminalControls = useRef(new Map());
  const remote = useRef(),
    activeRef = useRef(active),
    lastBrowserTab = useRef(""),
    readyRef = useRef(false),
    browserIds = useRef(new Set()),
    initialized = useRef(false),
    browserStreamReady = useRef(false),
    browserSnapshot = useRef({ tabs: [] }),
    refreshStatus = useRef(null),
    launchAcknowledged = useRef(false),
    lastJob = useRef(""),
    quitAfterStop = useRef(null);
  activeRef.current = active;
  readyRef.current = Boolean(status?.ready);
  const storageRecovery = status?.storage_recovery?.required;
  const current = tabs.find((t) => t.id === active),
    terminal = !desktopOpen && current?.kind === "terminal",
    busy = Boolean(status?.job?.running),
    showHome = storageRecovery || !desktopOpen && (home || !status?.vm_running || !current);
  const setupProgress = status?.job?.setup_progress && (busy || status.job.error);
	const cliSessions = useCLISessions(Boolean(status && !busy && !storageRecovery), windowVisible);
  useEffect(() => {
    if (storageRecovery) {
      setDesktopOpen(false);
      setHome(true);
      setStartOptions(null);
      setStartRequest(null);
    }
  }, [storageRecovery]);
  const other = tabs.find(tab => tab.id === layout.other);
  const split = Boolean(other && current && !showHome && !desktopOpen);
  const panes = !split ? [current].filter(Boolean)
    : layout.side === "left" ? [current, other] : [other, current];
  const webPanes = panes.filter(tab => tab.kind !== "terminal");
  const passwordVault = usePasswordVault(Boolean(status?.ready), browser.active);
  const dualBrowser = split && webPanes.length === 2;
  const leftId = dualBrowser ? panes[0].id : "";
  const rightId = dualBrowser ? panes[1].id : "";
  const nativeSplit = browser.split;
  const splitReady = dualBrowser
    ? nativeSplit?.left === leftId && nativeSplit?.right === rightId
    : !nativeSplit;
  const liveBrowser = webPanes.find(tab => tab.id === browser.active) || webPanes[0];
  const liveBrowserVisible = !desktopOpen && !showHome && status?.ready &&
    liveBrowser && splitReady && (!split || browser.active === liveBrowser.id);
  const viewBeforeBrowser = useRef(null);
  const browserDismissed = useRef(false);
  const browserVisible = !desktopOpen && !showHome && webPanes.length > 0;
  const browserWasVisible = useRef(false);
  useEffect(() => {
    // Preserve the return destination until the last tab's removal is handled,
    // including removals received through events or fallback polling.
    if (browserWasVisible.current && !tabs.some(tab => tab.kind !== "terminal")) closeBrowser();
    else if (!browserVisible) viewBeforeBrowser.current = {
      layout, home: showHome, desktopOpen, selectedProject,
    };
    browserWasVisible.current = browserVisible;
  }, [browserVisible, layout, showHome, desktopOpen, selectedProject, tabs]);
  const closeBrowser = () => {
    const previous = viewBeforeBrowser.current;
    browserDismissed.current = true;
    const available = id => tabs.some(tab => tab.id === id);
    const restored = previous?.layout;
    const id = [restored?.active, restored?.other].find(available);
    setLayout(id ? { ...restored, active: id,
      other: restored.other !== id && available(restored.other) ? restored.other : undefined,
    } : { active });
    setHome(Boolean(previous?.home || !previous?.desktopOpen && !id));
    setDesktopOpen(Boolean(previous?.desktopOpen));
    if (previous?.selectedProject) setSelectedProject(previous.selectedProject);
  };
  const splitQueue = useRef(Promise.resolve());
  const requestedSplit = useRef(false);
  const nativeSplitRef = useRef(null);
  nativeSplitRef.current = nativeSplit;
  useEffect(() => {
    if (!status?.ready) { requestedSplit.current = false; return; }
    if (!leftId && !requestedSplit.current && !nativeSplitRef.current) return;
    requestedSplit.current = Boolean(leftId);
    let cancelled = false;
    const apply = async () => {
      if (cancelled) return;
      const id = activeRef.current;
      try {
        const result = await api("/browser/split", { left: leftId, right: rightId,
          id: leftId ? (id === rightId ? rightId : leftId) : id });
        if (!cancelled) setBrowser(previous => ({ ...previous, split: result.split }));
      } catch {
        if (!cancelled) {
          setToast(tabs.some(tab => [leftId, rightId].includes(tab.id) && tab.incognito)
            ? "无痕标签可与终端分屏；两个网页分屏目前仅支持普通标签。"
            : "无法调整浏览器分屏。请确认环境组件已更新，两个网页属于同一浏览器配置，且客机 Chromium 为 155 或更高版本。");
          setLayout(previous => ({ active: previous.active }));
        }
      }
    };
    splitQueue.current = splitQueue.current.then(apply);
    return () => { cancelled = true; };
  }, [leftId, rightId, status?.ready, Boolean(!leftId && nativeSplit)]);
  const layoutRef = useRef(layout);
  layoutRef.current = layout;
  useEffect(() => {
    if (!layout.other) return;
    if (!status?.vm_running || !other || !current) {
      const survivor = current || other;
      setLayout({ active: survivor?.id || "" });
      if (!current && survivor && survivor.kind !== "terminal" && status?.ready) {
        api("/browser/activate", { id: survivor.id }).catch(error => setToast(error.message));
      }
    }
  }, [layout, other, current, status?.vm_running, status?.ready]);
  const paneSide = id => !split ? "" : id === active ? layout.side :
    layout.side === "left" ? "right" : "left";
  const running = Boolean(status?.vm_running);
  const mixedBrowserId = split && !dualBrowser ? webPanes[0]?.id : null;
  useEffect(() => {
    if (status?.ready && mixedBrowserId && mixedBrowserId !== browser.active)
      api("/browser/activate", { id: mixedBrowserId }).catch(error => setToast(error.message));
  }, [mixedBrowserId, status?.ready, browser.active]);
  const finishedLogins = tabs.filter((tab) => tab.terminalKind === "claude-login" && tab.closed).map((tab) => tab.id).sort().join(",");
  useEffect(() => {
    if (finishedLogins) claudeProfiles.load();
  }, [finishedLogins, claudeProfiles.load]);
  const audio = useAudioOutput(running && !storageRecovery && !status?.environment_update_required && !connectionError);
  useEffect(() => {
    if (audio.error) setToast(audio.error);
  }, [audio.error]);
  const projects = status?.projects || [];
  const workspace = selectedProject || status?.workspace;
  const contentWorkspace =
    terminal && !showHome ? current.workspace : workspace;
  const setupProxy = !nativeGuestLabel && needsProxySetup(settings, status, proxy);
  const network = connectionError
    ? { mode: "unknown", label: "外网状态待确认" }
    : networkState(status);
  const runtimeLabel = connectionError
    ? "连接中断"
    : busy
      ? network.mode === "checking" || status?.job?.kind === "safety"
        ? "检查中"
        : status?.job?.kind === "stop"
          ? "停止中"
          : running
            ? "应用中"
            : "启动中"
      : status?.ready
        ? network.mode === "open"
          ? "运行中"
          : "离线可用"
        : running
          ? status?.environment_update_required ? "环境待更新" : "浏览器待恢复"
          : "已停止";
  const notify = (text) => setToast(text);
  const clientNow = getClientContext();
  const readonlyStreamKey = `${transportGeneration(clientNow)}:${clientNow.connection?.state || ""}:${clientNow.connection_id || ""}`;
  useEffect(() => {
    if (!status?.ready || !nativeAvailable() || getClientContext().connection?.state === "reconnecting") return;
    const clipboard = shareClipboard({
      connect: () => new WebSocket(runtimeSocketURL("/clipboard")), native,
      onStatus: setToast,
      setActive: active => { window.cageSharedClipboardActive = active; },
    });
    window.cageSyncClipboard = () => clipboard.sync();
    return () => { clipboard.dispose(); delete window.cageSyncClipboard; };
  }, [status?.ready, readonlyStreamKey]);
  const desktopFiles = useDesktopFiles(Boolean(status?.ready), notify);
  const downloads = [...(browser.downloads || []), ...desktopFiles.downloads];
  useDownloadNotifications(downloads, notify, Boolean(browser.uploads?.length || desktopFiles.uploads.length));
  const refreshSettings = async () => {
    const results = await Promise.allSettled([api("/settings"), api("/proxy")]);
    if (results[0].status === "fulfilled") setSettings(results[0].value);
    else notify(results[0].reason.message);
    if (results[1].status === "fulfilled") setProxy(results[1].value);
    else setProxy(null);
  };
  const confirm = (options) =>
    new Promise((resolve) => setDialog({ ...options, resolve }));
  const openSettings = (section = "keyboard") => {
    setSafetyOpen(false);
    setSettingsSection(section);
    setSettingsOpen(true);
  };
  const select = async (id, focusContent = true) => {
    const tab = tabs.find((t) => t.id === id);
    if (tab && tab.kind !== "terminal") browserDismissed.current = false;
    setDesktopOpen(false);
    setHome(false);
    if (tab?.kind === "browser-settings") setLayout({ active: id });
    else setActive(id);
    if (!tab?.incognito) localStorage.setItem(clientStorageKey("cage-active"), id);
    if (tab?.kind === "terminal") setSelectedProject(tab.workspace);
    if (tab && tab.kind !== "terminal") {
      try {
        await api("/browser/activate", { id });
        if (activeRef.current === id) {
          setBrowser(previous => ({ ...previous, active: id }));
          if (focusContent) remote.current?.focus();
        }
      } catch (e) {
        notify(e.message);
      }
    }
  };
  const destination = (id, x, y) => {
    const box = workspaceBox.current?.getBoundingClientRect();
    if (!box || busy || !running || desktopOpen || tabs.length < 2 ||
        document.elementFromPoint(x, y)?.closest(".chrome") ||
        !tabs.some(tab => tab.id === id) || tabs.find(tab => tab.id === id)?.kind === "browser-settings" ||
        current?.kind === "browser-settings" || x < box.left || x > box.right ||
        y < box.top || y > box.bottom) return null;
    return x < box.left + box.width * (dualBrowser && nativeSplit?.fraction || 0.5) ? "left" : "right";
  };
  const splitDrag = {
    clear: () => setDropSide(null),
    hover(id, x, y) {
      const side = destination(id, x, y);
      setDropSide(side);
      return side;
    },
    drop(id, x, y) {
      const side = destination(id, x, y);
      if (!side) return false;
      setLayout(previous => splitTab(previous, tabs, id, side));
      void select(id);
      return true;
    },
  };
  const action = async (path, body = {}) => {
    try {
      return await api(path, body);
    } catch (e) {
      notify(e.message);
      return null;
    }
  };
  const openBrowserPage = async (
    url = settings?.browser_home_url || "https://www.bing.com/",
    incognito = Boolean(current?.incognito),
  ) => {
    browserDismissed.current = false;
    const result = await action("/browser/new", { url, incognito });
    if (result) {
      setDesktopOpen(false);
      setHome(false);
      setTabs((list) =>
        list.some((t) => t.id === result.id)
          ? list
          : [
              ...list,
              {
                id: result.id,
                incognito,
                kind: "web",
                url,
                title: "新标签页",
              },
            ],
      );
      setActive(result.id);
    }
    return result;
  };
  const newPage = () => openBrowserPage();
  const showBrowserResult = result => {
    if (!result?.id) return;
    browserDismissed.current = false;
    setDesktopOpen(false); setHome(false);
    setTabs(previous => previous.some(tab => tab.id === result.id)
      ? previous.map(tab => tab.id === result.id ? { ...tab, ...result } : tab)
      : [...previous, result]);
    setLayout({ active: result.id });
  };
  const openBrowserSettings = async (section = "general") => {
    const result = await action("/browser/settings", { mode: section });
    if (result) showBrowserResult(result);
  };
  const openInternalPage = async (mode, id) => {
    const result = await action("/browser/internal-page", { mode, id });
    if (result) showBrowserResult(result);
  };
  const openSettingsPageResult = result => result.id ? showBrowserResult(result) : openBrowserPage(result.url, false);
  const openBrowser = () => {
    const tab =
      tabs.find(
        (t) => t.id === lastBrowserTab.current && t.kind !== "terminal",
      ) ||
      tabs.find((t) => t.id === browser.active && t.kind !== "terminal") ||
      tabs.find((t) => t.kind !== "terminal");
    return tab ? select(tab.id) : newPage();
  };
  const newTerminal = (path = workspace, anchor = terminalTrigger.current) =>
    setTerminalChoice((previous) =>
      previous?.anchor === anchor ? null : { path, anchor },
    );
  const createTerminal = async (path, kind, loginAccountId) => {
    setTerminalChoice(null);
    const result = await api("/terminals", {
      workspace: path, kind,
      ...(loginAccountId && { claude_account_id: loginAccountId }),
    });
    showTerminal(result);
    sessionsChanged();
    if (kind === "claude" && result.claude_account_id &&
        !claudeProfiles.accounts?.some((profile) => profile.id === result.claude_account_id)) {
      claudeProfiles.load();
    }
  };
  const showTerminal = result => {
    const tab = {
      ...result, id: "terminal:" + result.id, sessionId: result.id,
      terminalKind: result.kind, kind: "terminal",
    };
    setTabs((list) => [...list.filter((t) => t.id !== tab.id && (!tab.cli_session_id || t.cli_session_id !== tab.cli_session_id)), tab]);
    setSelectedProject(result.workspace);
    setDesktopOpen(false);
    setHome(false);
    setActive(tab.id);
  };
  const openSessionTerminal = async session => {
    if (session.running) return;
    if (sessionOpenRequest.current?.id === session.id) return;
    sessionOpenRequest.current?.controller.abort();
    const controller = new AbortController();
    const request = { id: session.id, controller };
    sessionOpenRequest.current = request;
    setOpeningSession(session.id);
    setSessionOpenError(null);
    const timer = setTimeout(() => controller.abort(new Error("打开会话超时，历史已保留。请重新打开；若仍失败，请在设置中导出诊断包。")), 60000);
    try {
      const view = await api("/cli/sessions/" + session.id + "/open", {}, undefined, {}, { signal: controller.signal });
      const terminals = await api("/terminals", undefined, undefined, {}, { signal: controller.signal });
      if (sessionOpenRequest.current !== request) return;
      const terminal = terminals.find(item => item.id === view.desktop_terminal_id && item.cli_session_id === session.id);
      if (!terminal) throw new Error("终端状态已变化，请重新打开会话");
      showTerminal(terminal);
      sessionsChanged();
    } catch (error) {
      if (sessionOpenRequest.current === request && error.code !== "connection_changed") {
        const message = (controller.signal.aborted ? controller.signal.reason : error).message;
        setSessionOpenError({ id: session.id, message });
      }
    } finally {
      clearTimeout(timer);
      if (sessionOpenRequest.current === request) {
        sessionOpenRequest.current = null;
        setOpeningSession("");
      }
    }
  };
  const archiveSession = async (session, anchor) => {
    await confirm({title:"归档这段会话？", body:"归档会关闭关联终端。历史将保留，可从设置中的已归档会话恢复。", confirm:"归档会话", pendingLabel:"正在归档…", anchor,
      submit: async () => {
        await api("/cli/sessions/" + session.id + "/archive", {});
        setTabs(list => list.filter(tab => tab.cli_session_id !== session.id));
        sessionsChanged();
        refreshStatus.current?.();
      },
    });
  };
  const pinSession = async session => {
    try {
      await api("/cli/sessions/" + session.id + "/metadata", {pinned:!session.pinned});
      sessionsChanged();
    } catch (error) { notify(error.message); }
  };
  const newTerminalHere = (event) =>
    newTerminal(contentWorkspace, event?.currentTarget?.tagName === "BUTTON"
      ? event.currentTarget : terminalTrigger.current);
  const terminalMenuAt = (location) => terminalChoice?.anchor?.dataset.terminalLocation === location && (
    <TerminalMenu choice={terminalChoice} busy={busy}
      onClose={() => setTerminalChoice(null)}
      onSelect={(kind) => createTerminal(terminalChoice.path, kind).catch((e) => notify(e.message))} />
  );
  const openEditor = async (path = workspace) => {
    browserDismissed.current = false;
    const result = await action("/browser/editor", { workspace: path });
    if (result) {
      setDesktopOpen(false);
      setHome(false);
      setTabs((list) =>
        list.some((t) => t.id === result.id)
          ? list
          : [
              ...list,
              {
                id: result.id,
                kind: "editor",
                workspace: path,
                url: `http://127.0.0.1:18080/?folder=${encodeURIComponent(path)}`,
                title: "文件编辑器",
              },
            ],
      );
      setActive(result.id);
    }
  };
  const closeTab = async (tab, anchor) => {
    if (tab.cli_session_id) return archiveSession({id:tab.cli_session_id, title:tab.title}, anchor);
    if (tab.kind === "terminal") {
      if (
        !tab.closed &&
        !(await confirm({
          title: "关闭这个终端会话？",
          body: "该终端中的交互命令可能被中断。网页和其他终端不受影响。",
          confirm: "关闭终端",
        }))
      )
        return;
      try {
        await api("/terminals/" + tab.sessionId, undefined, "DELETE");
      } catch (e) {
        notify(e.message);
        return;
      }
    } else {
      if (
        tab.kind === "editor" &&
        !(await confirm({
          title: "关闭文件编辑器？",
          body: "请先保存未完成的修改。文件仍保留在共享项目中。",
          confirm: "关闭编辑器",
        }))
      )
        return;
      if (!(await action("/browser/close", { id: tab.id }))) return;
    }
    setTabs((list) => list.filter((t) => t.id !== tab.id));
    if (tab.incognito) setPrivateTabMetadata(previous => Object.fromEntries(Object.entries(previous).map(([key, value]) => [key,
      Object.fromEntries(Object.entries(value).filter(([id]) => id !== tab.id).map(([id, item]) =>
        [id, id === "order" ? item.filter(id => id !== tab.id) : item])),
    ])));
    // The browser visibility effect restores the previous view for an empty
    // browser. Do not overwrite it when a close response follows its event.
    if (tab.kind !== "terminal" && !tabs.some(item => item.id !== tab.id && item.kind !== "terminal")) return;
    if (split && (tab.id === active || tab.id === other.id)) {
      const remaining = tab.id === active ? other.id : active;
      setLayout({ active: remaining });
      await select(remaining);
    } else if (active === tab.id) {
      const group = displayTabs.filter((item) =>
        tab.kind === "terminal"
          ? item.kind === "terminal" && item.workspace === tab.workspace
          : item.kind !== "terminal",
      );
      const at = group.findIndex((item) => item.id === tab.id);
      const next = group[at + 1] || group[at - 1];
      if (next) await select(next.id);
      else setActive("");
    }
  };
  const navigate = async (id, address) => {
    try {
      const internal = /^cage:\/\/settings(?:#(general|appearance|privacy|extensions|about))?\/?$/.exec(address.trim());
      if (internal) { await openBrowserSettings(internal[1] || "general"); return; }
      const url = normalizeURL(address);
      if (network.mode !== "open" && !isGuestLocalURL(url)) {
        setSafetyOpen(true);
        notify("请先完成关键检查，再访问外部网站。");
        return;
      }
      await action("/browser/navigate", { id, url });
      remote.current?.focus();
    } catch (error) {
      notify(error.message);
    }
  };
  const stop = async () => {
    if (
      await confirm({
        title: setupProgress ? "停止安装环境？" : "停止当前环境？",
        body: setupProgress ? "当前安装任务会停止。下次启动环境时可以继续安装。" : "请先保存文件。当前终端会话会结束，浏览器登录和项目文件会保留。",
        confirm: setupProgress ? "停止安装" : "停止环境",
      })
    )
      await action("/stop");
  };
  const cancelAutomaticExit = () => {
    quitAfterStop.current = null;
    setQuitting(null);
  };
  const stopAndQuit = async () => {
    const request = { id: null };
    quitAfterStop.current = request;
    setQuitting({ error: "" });
    try {
      await stopAndExit({
        stop: () => api("/stop", {}),
        readStatus: () => api("/status"),
        exit: () => native("quitNow"),
        isCancelled: () => quitAfterStop.current !== request,
        onStatus: setStatus,
      });
    } catch (error) {
      if (quitAfterStop.current === request) {
        quitAfterStop.current = null;
        setQuitting({ error: error.message });
      }
    }
  };
  useEffect(() => {
    let live = true,
      timer,
      polling = false;
    const poll = async () => {
      if (!live || polling) return;
      clearTimeout(timer);
      polling = true;
      let jobRunning = false;
      try {
        const next = await api("/status");
        if (!live) return;
        jobRunning = next.job.running;
        setStatus(next);
        setStartRequest(pending => pending?.id && pending.id === next.job.id ? null : pending);
        setConnectionError("");
        if (
          !next.job.running &&
          next.job.id &&
          lastJob.current !== next.job.id
        ) {
          lastJob.current = next.job.id;
          await refreshSettings();
          if (next.job.kind === "guest-install" && !next.job.error) {
            const install = await api("/environment/guest-systems");
            if (live && install.activate) setGuestSystem(previous => ({ ...previous, activate:true }));
          }
          if (next.job.error) notify(next.job.error);
        }
        if (next.ready) {
          const fetchBrowser = !browserStreamReady.current || !initialized.current;
          const [browserResult, t] = await Promise.all([
            fetchBrowser ? api("/browser/tabs").then(result => {
              if (live) setBrowserError("");
              return result;
            }).catch(error => {
              if (live) setBrowserError(error.message);
              return null;
            }) : null,
            api("/terminals"),
          ]);
          if (!live) return;
          const b = browserResult || browserSnapshot.current;
          const legacyMetadata = readLocal(clientStorageKey("cage-tab-layout"), {})[next.environment_identity_directory || next.environment_directory || next.workspace] || {};
          for (const item of t) {
            const saved = legacyMetadata["terminal:" + item.id];
            if (!item.cli_session_id || item.metadata_set || !saved || (!saved.title && !saved.pinned)) continue;
            try {
              const updated = await api("/terminals/" + item.id + "/metadata", {
                ...(saved.title ? {title:saved.title} : {}), pinned:Boolean(saved.pinned),
              });
              Object.assign(item, {title:updated.title, custom_title:updated.custom_title, pinned:updated.pinned, metadata_set:true});
            } catch { /* Keep the existing local label until the host accepts it. */ }
          }
          if (!live) return;
          browserSnapshot.current = b;
          setBrowser(b);
          const all = [
            ...b.tabs,
            ...t.map((item) => ({
              ...item,
              id: "terminal:" + item.id,
              sessionId: item.id,
              terminalKind: item.kind,
              kind: "terminal",
            })),
          ];
          setTabs((previous) => {
            const map = new Map(all.map((item) => [item.id, item]));
            return [
              ...previous
                .filter((item) => map.has(item.id))
                .map((item) => map.get(item.id)),
              ...all.filter((item) => !previous.some((p) => p.id === item.id)),
            ];
          });
          if (!browserDismissed.current && initialized.current && b.active && b.active !== activeRef.current && !browserIds.current.has(b.active)) {
            setDesktopOpen(false);
            setHome(false);
          }
          if (
            !activeRef.current ||
            !all.some((item) => item.id === activeRef.current) ||
            (!browserDismissed.current && initialized.current &&
              b.active &&
              !browserIds.current.has(b.active))
          ) {
            if (browserDismissed.current && !all.some(item => item.id === activeRef.current)) setHome(true);
            // Let split reconciliation promote the remaining pane on close.
            if (!layoutRef.current.other || all.some(item => item.id === activeRef.current))
              setActive(b.active || all[0]?.id || "");
          }
          browserIds.current = new Set(b.tabs.map((item) => item.id));
          initialized.current = true;
        }
      } catch (e) {
        if (live) setConnectionError(e.message);
      } finally {
        polling = false;
        if (live)
          timer = setTimeout(
            poll,
            jobRunning ? 1200 : windowVisibleRef.current ? 3000 : 15000,
          );
      }
    };
    (async () => {
      try {
        const token = location.hash.slice(1);
        if (token) {
          await api("/session", { token });
          history.replaceState(null, "", location.pathname);
        }
        const config = await api("/settings");
        window.dispatchEvent(new Event("cage-session-ready"));
        if (!live) return;
        setSettings(config);
        try {
          const proxyState = await api("/proxy");
          if (!live) return;
          setProxy(proxyState);
        } catch {
          // Network settings remain available when the core cannot be read.
        }
        refreshStatus.current = poll;
        await poll();
      } catch (e) {
        if (live) setFatal(e.message);
      }
    })();
    return () => {
      live = false;
      refreshStatus.current = null;
      clearTimeout(timer);
    };
  }, []);
  useEffect(() => {
    if (windowVisible) refreshStatus.current?.();
  }, [windowVisible]);
  useEffect(() => {
    if (!settings || !status || fatal || connectionError || launchAcknowledged.current || !nativeAvailable()) return;
    // Effects run after the workbench render commits. HTML navigation alone
    // cannot confirm a successful upgrade when session/API startup failed.
    let live = true;
    native("workbenchReady").then((accepted) => {
      if (live && accepted === true) launchAcknowledged.current = true;
    }).catch(() => {});
    return () => { live = false; };
  }, [settings, status, fatal, connectionError]);
  useEffect(() => {
    if (!status?.ready || getClientContext().connection?.state === "reconnecting") return;
    const stream = new EventSource(runtimeURL("/browser/events"));
    stream.addEventListener("clipboard", (event) => {
      remote.current?.receiveCopy(JSON.parse(event.data));
    });
    stream.onopen = () => { browserStreamReady.current = true; };
    stream.onerror = () => { browserStreamReady.current = false; };
    stream.onmessage = (event) => {
      const b = JSON.parse(event.data);
      setBrowserError("");
      if (b.desktopFocus === true) {
        setDesktopOpen(true);
        setHome(false);
        setSettingsOpen(false);
      }
      const previousPair = browserSnapshot.current.split;
      const local = layoutRef.current;
      if (previousPair && !b.split && [local.active, local.other].includes(previousPair.left) &&
          [local.active, local.other].includes(previousPair.right)) {
        const survivor = b.tabs.some(tab => tab.id === local.active) ? local.active : local.other;
        setLayout({ active: survivor });
      }
      browserSnapshot.current = b;
      setBrowser((previous) => ({ ...previous, ...b }));
      setTabs((previous) => {
        const map = new Map(b.tabs.map((t) => [t.id, t]));
        return [
          ...previous
            .filter((t) => t.kind === "terminal" || map.has(t.id))
            .map((t) => map.get(t.id) || t),
          ...b.tabs.filter((t) => !previous.some((p) => p.id === t.id)),
        ];
      });
      if (!browserDismissed.current && b.active && b.focus) {
        setDesktopOpen(false);
        setHome(false);
      }
      if (
        b.active &&
        ((!browserDismissed.current && b.focus) ||
          !activeRef.current ||
          (!layoutRef.current.other && !activeRef.current.startsWith("terminal:")))
      )
        setActive(b.active);
      browserIds.current = new Set(b.tabs.map((t) => t.id));
    };
    return () => {
      browserStreamReady.current = false;
      stream.close();
    };
  }, [status?.ready, readonlyStreamKey]);
  useEffect(() => {
    if (current && current.kind !== "terminal")
      lastBrowserTab.current = current.id;
    document.title =
      (current?.title && current.title !== "新标签页"
        ? current.title + " · "
        : "") + "Cage";
    if (active && !current?.incognito) localStorage.setItem(clientStorageKey("cage-active"), active);
  }, [current?.url, current?.title, current?.incognito, active]);
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(""), 4500);
    return () => clearTimeout(timer);
  }, [toast]);
  useEffect(() => {
    const key = (event) => {
      if (document.querySelector("[data-environment-dialog]") || !primaryModifier(event)) return;
      const k = event.key.toLowerCase();
      if (document.querySelector("[data-browser-dialog], .mac-login-fullscreen")) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          document.dispatchEvent(new Event("cage-close-modal"));
        }
        return;
      }
      if (dialog) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          void finishDialog(false);
        }
        return;
      }
      if (k === "," && !projectDialog && !rename && !hostGitOpen) {
        event.preventDefault();
        event.stopImmediatePropagation();
        openSettings();
        return;
      }
      if (
        settingsOpen ||
        hostGitOpen ||
        transfer ||
        details ||
        safetyOpen ||
        projectDialog ||
        terminalChoice ||
        rename
      ) {
        if (k === "w") {
          event.preventDefault();
          event.stopImmediatePropagation();
          document.dispatchEvent(new Event("cage-close-modal"));
        }
        return;
      }
      let handle;
      if (k === "o") {
        event.preventDefault();
        event.stopImmediatePropagation();
        if (event.shiftKey) {
          document.dispatchEvent(new Event("cage-show-favorites"));
          return;
        }
        if (!busy && running) setProjectDialog({ path: "" });
        return;
      }
      if (k === "t" && !event.shiftKey) handle = newPage;
      if (k === "n" && event.shiftKey) handle = () => openBrowserPage(undefined, true);
      if (k === "d") handle = () => document.dispatchEvent(new Event("cage-add-favorite"));
      if (k === "enter" && event.shiftKey) handle = newTerminalHere;
      if (k === "l")
        handle = () => {
          const input = [...document.querySelectorAll("[data-browser-tab]")].find(node => node.dataset.browserTab === active)?.querySelector(".address-bar input");
          input?.focus();
          input?.select();
        };
      if (k === "w" && current && !desktopOpen) handle = () => closeTab(current);
      if (/^[1-9]$/.test(k)) {
        const tab = k === "9" ? switchTabs.at(-1) : switchTabs[Number(k) - 1];
        if (tab) handle = () => select(tab.id);
      }
      if (event.shiftKey && ["[", "]", "{", "}"].includes(k)) {
        const direction = ["[", "{"].includes(k) ? -1 : 1,
          at = switchTabs.findIndex((t) => t.id === active),
          tab =
            switchTabs[
              (at + direction + switchTabs.length) % switchTabs.length
            ];
        if (tab) handle = () => select(tab.id);
      }
      if (handle) {
        event.preventDefault();
        event.stopImmediatePropagation();
        if (
          !busy &&
          (status?.ready ||
            (running && k === "enter") ||
            (k === "w" && terminal))
        )
          void handle();
      }
    };
    window.addEventListener("keydown", key, true);
    window.cageEditAction = (command) => {
      if (document.querySelector(".mac-login-fullscreen")) return false;
      if (
        settingsOpen ||
        transfer ||
        dialog ||
        details ||
        safetyOpen ||
        projectDialog ||
        terminalChoice ||
        rename ||
        showHome
      )
        return false;
      if (terminal)
        return terminalControls.current.get(current.id)?.edit(command) || false;
      return remote.current?.edit(command) || false;
    };
    window.cageWorkbenchAction = async (command) => {
      if (document.querySelector(".mac-login-fullscreen")) {
        if (command === "closeTab" || command === "quit") document.dispatchEvent(new Event("cage-close-modal"));
        if (command !== "quit") return;
      }
      if (document.querySelector("[data-environment-dialog]")) {
        if (command === "closeTab") document.dispatchEvent(new Event("cage-close-modal"));
        return;
      }
      if (hostGitOpen && command !== "quit") {
        if (command === "closeTab")
          document.dispatchEvent(new Event("cage-close-modal"));
        return;
      }
      if (command === "settings") {
        if (!projectDialog && !rename && !dialog) openSettings();
        return;
      }
      if (storageRecovery && ["openProject", "newTab", "newTerminal"].includes(command)) {
        openSettings("project");
        return;
      }
      if (command === "openProject") {
        if (
          !busy &&
          running &&
          !settingsOpen &&
          !dialog &&
          !projectDialog &&
          !rename
        )
          setProjectDialog({ path: "" });
        return;
      }
      if (
        (settingsOpen || dialog || projectDialog || terminalChoice || rename) &&
        ["newTab", "newTerminal"].includes(command)
      )
        return;
      if (command === "newTab" && status?.ready) await newPage();
      if (command === "newTerminal" && status?.vm_running)
        await newTerminalHere();
      if (command === "closeTab") {
        if (document.querySelector("[data-browser-dialog], .mac-login-fullscreen")) {
          document.dispatchEvent(new Event("cage-close-modal"));
          return;
        }
        if (dialog) {
          void finishDialog(false);
        } else if (
          settingsOpen ||
          transfer ||
          details ||
          safetyOpen ||
          projectDialog ||
          terminalChoice ||
          rename
        )
          document.dispatchEvent(new Event("cage-close-modal"));
        else if (desktopOpen) setDesktopOpen(false);
        else if (current) await closeTab(current);
      }
      if (command === "quit") {
        if (quitAfterStop.current) return;
        if (isRemoteClient()) {
          await native("quitNow");
          return;
        }
        if (status && !connectionError && !status.vm_running && !busy) {
          await native("quitNow");
          return;
        }
        if (connectionError || fatal) {
          setDialog({
            title: "退出 Cage",
            body: "工作台连接已中断，暂时无法确认环境状态。仍在运行的后台任务会继续。",
            confirm: "仅退出桌面应用",
            resolve: (confirmed) => confirmed && native("quitNow"),
          });
          return;
        }
        setDialog({
          title: "退出 Cage",
          body: "退出前请保存文件和设置。可以保留环境，让后台任务继续运行；下次打开会重新连接。",
          confirm: "停止环境并退出",
          secondary: "保留运行并退出",
          resolve: async (result) => {
            if (result === "secondary") {
              await native("quitNow");
            } else if (result) {
              await stopAndQuit();
            }
          },
        });
      }
    };
    return () => {
      window.removeEventListener("keydown", key, true);
      delete window.cageWorkbenchAction;
      delete window.cageEditAction;
    };
  }, [
    status,
    settingsOpen,
    hostGitOpen,
    safetyOpen,
    details,
    transfer,
    dialog,
    current,
    tabs,
    busy,
    active,
    connectionError,
    fatal,
    projectDialog,
    rename,
    showHome,
    savedTabs,
    selectedProject,
    desktopOpen,
    terminalChoice,
  ]);
  const project = projectName(contentWorkspace, status?.environment_directory);
  const tabLayoutKey = status?.environment_identity_directory || status?.environment_directory || status?.workspace;
  const metadata = { ...(savedTabs[tabLayoutKey] || {}), ...(privateTabMetadata[tabLayoutKey] || {}) };
  for (const tab of tabs) {
    if (tab.cli_session_id && tab.metadata_set) metadata[tab.id] = {
      ...metadata[tab.id], title:tab.custom_title || tab.title, pinned:Boolean(tab.pinned),
    };
  }
  const claudeLabels = new Map(claudeProfiles.accounts?.map((account) => [account.id, account.label || account.email]));
  const claudeNumbers = new Map(claudeProfiles.accounts?.map((account, index) => [account.id, index + 1]));
  const terminalAccountName = (tab) => claudeLabels.get(tab.claude_account_id) ||
    (tab.terminalKind === "claude-login" ? "新账号登录" : tab.claude_account_id ? "Claude 账号" : "");
  const displayTabs = orderedTabs(tabs, metadata).map((tab) =>
    tab.claude_account_id || tab.terminalKind === "claude-login" ? { ...tab,
      claudeLabel: terminalAccountName(tab), claudeAccountNumber: claudeNumbers.get(tab.claude_account_id) } : tab);
  const browserTabs = displayTabs.filter((tab) => tab.kind !== "terminal");
  const switchTabs =
    terminal && !showHome
      ? displayTabs.filter(
          (tab) =>
            tab.kind === "terminal" && tab.workspace === current.workspace,
        )
      : dualBrowser ? browserTabs.filter(tab => current.id === rightId ? tab.id === rightId : tab.id !== rightId) : browserTabs;
  const renameTab = (tab) =>
    setRename({ id: tab.id, title: tabName({ ...tab, incognito: false }, metadata, project) });
  const updateSharedMetadata = async (tab, changes) => {
    const updated = await api("/terminals/" + tab.sessionId + "/metadata", changes);
    setTabs(list => list.map(item => item.id === tab.id ? {
      ...item, title:updated.title, custom_title:updated.custom_title, pinned:updated.pinned, metadata_set:true,
    } : item));
  };
  const pinTab = (id) => {
    const tab = tabs.find(tab => tab.id === id);
    if (tab?.cli_session_id) {
      void updateSharedMetadata(tab, {pinned:!metadata[id]?.pinned}).catch(error => notify(error.message));
      return;
    }
    changeTabMetadata({
      ...metadata,
      [id]: { ...metadata[id], pinned: !metadata[id]?.pinned },
    });
  };
  const persist = (key, value, setter) => {
    try {
      localStorage.setItem(clientStorageKey(key), JSON.stringify(value));
      setter(value);
    } catch {
      notify("未能保存界面偏好，请检查本地存储空间。");
    }
  };
  const changeTabMetadata = (next) => {
    const currentPrivateIDs = new Set(tabs.filter(tab => tab.incognito).map(tab => tab.id));
    const privateIDs = new Set([...currentPrivateIDs, ...Object.keys(privateTabMetadata[tabLayoutKey] || {}).filter(id => id !== "order" && id !== "sessionOrder")]);
    const persistent = Object.fromEntries(Object.entries(next).filter(([id]) => !privateIDs.has(id)));
    if (next.order) persistent.order = next.order.filter(id => !privateIDs.has(id) && tabs.some(tab => tab.id === id));
    setPrivateTabMetadata(previous => ({ ...previous, [tabLayoutKey]: {
      ...Object.fromEntries(Object.entries(next).filter(([id]) => currentPrivateIDs.has(id))),
      ...(next.order ? { order: next.order } : {}),
    } }));
    persist(
      "cage-tab-layout",
      { ...savedTabs, [tabLayoutKey]: persistent },
      setSavedTabs,
    );
  };
  const reorder = (from, to, scopeOrder) => {
    if (scopeOrder) {
      const ids = new Set(scopeOrder);
      let index = 0;
      const order = displayTabs.map((tab) => ids.has(tab.id) ? scopeOrder[index++] : tab.id);
      changeTabMetadata({ ...metadata, order });
      return;
    }
    if (from === to) return;
    const order = displayTabs.map((tab) => tab.id).filter((id) => id !== from);
    order.splice(order.indexOf(to), 0, from);
    changeTabMetadata({ ...metadata, order });
  };
  const openedProject = async (path) => {
    await refreshSettings();
    await refreshStatus.current?.();
    setSelectedProject(path);
    setProjectDialog(null);
    setHome(true);
  };
  useEffect(() => {
    if (status?.workspace) setSelectedProject(status.workspace);
  }, [status?.workspace]);
  const finishDialog = async (value) => {
    if (!dialog || dialog.pending) return;
    if (value === true && dialog.submit) {
      setDialog({ ...dialog, pending: true, error: "" });
      try { await dialog.submit(); }
      catch (error) {
        setDialog({ ...dialog, pending: false, error: error.message });
        return;
      }
    }
    const resolve = dialog.resolve;
    setDialog(null);
    resolve(value);
  };
  const sidebarButton = side => sidebarCollapsed && side !== "right" &&
    <ExpandSidebar onClick={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)} />;
  const expandButton = tab => split && tab && <ExpandPane onClick={() => {
    setLayout({ active: tab.id });
    void select(tab.id);
  }} />;
  const focusHeader = tab => {
    if (tab && tab.id !== activeRef.current) void select(tab.id, false);
  };
  const terminalHeader = tab => {
    const path = tab?.workspace || workspace;
    const account = tab?.claude_account_id && terminalAccountName(tab);
    const detail = tab?.terminalKind === "claude-login" ? `登录 · ${account || "新账号"}`
      : account ? `Claude · ${account}${tab.claude_account_source === "default" ? "（启动时账号）" : ""}` : "";
    const location = split && tab ? `heading-${tab.id}` : "heading";
    return <TerminalHeader tab={tab} project={projectName(path, status?.environment_directory)} metadata={metadata}
      contentWorkspace={path} terminalDetail={detail} terminalDetailHint={tab?.claude_account_source === "default"
        ? "此终端按启动时的普通终端登录标记。若普通终端已换号，请重新打开工作会话确认身份。" : detail}
      busy={busy} onHome={() => setHome(true)}
      sidebarButton={sidebarButton(paneSide(tab?.id))} exitButton={expandButton(tab)}
      location={location} triggerRef={!tab || tab.id === active ? terminalTrigger : undefined}
      menu={terminalMenuAt(location)} onClose={() => closeTab(tab)} />;
  };
  const recoverEditor = async tab => {
    if (!(await confirm({ title: "重启编辑器？", body: "请先保存文件。浏览器、终端和环境会继续运行。", confirm: "重启编辑器" }))) return;
    setRecoveringEditor(true);
    try { await api("/browser/editor-restart", { workspace: tab.workspace || workspace }); notify("编辑器已恢复"); }
    catch (error) { notify(error.message); }
    finally { setRecoveringEditor(false); }
  };
  const paneTabs = tab => !dualBrowser ? browserTabs : browserTabs.filter(item =>
    item.id === tab.id || (tab.id === leftId && item.id !== rightId));
  const browserHeader = tab => <BrowserHeader vault={passwordVault} onManageVault={() => openSettings("vault")} tab={tab} tabs={paneTabs(tab)} metadata={metadata}
    vertical={browserTabLayout === "vertical"}
    ready={status.ready} busy={busy} focused={active === tab.id} compact={split} splitDrag={splitDrag}
    sidebarButton={sidebarButton(paneSide(tab.id))} exitButton={expandButton(tab)}
    triggerRef={tab.id === active ? terminalTrigger : undefined}
    location={split ? `heading-${tab.id}` : "heading"} menu={terminalMenuAt(split ? `heading-${tab.id}` : "heading")}
    back={tab.kind !== "browser-settings" && (browser.split?.history?.[tab.id]?.back ?? (tab.id === browser.active && browser.back))}
    forward={tab.kind !== "browser-settings" && (browser.split?.history?.[tab.id]?.forward ?? (tab.id === browser.active && browser.forward))}
    action={action} onSelect={select} onClose={closeTab} onRename={renameTab} onPin={pinTab} onReorder={reorder}
    onNew={newPage} onNavigate={address => navigate(tab.id, address)} onOpen={openBrowserPage}
    onNewPrivate={() => openBrowserPage(undefined, true)} onNewRegular={() => openBrowserPage(undefined, false)}
    onSettings={openBrowserSettings}
    onReload={() => tab.kind === "browser-settings" ? setBrowserSettingsRevision(value => value + 1) : action("/browser/reload", { id: tab.id })}
    onCloseBrowser={!dualBrowser || tab.id === rightId ? closeBrowser : undefined}
    downloads={downloads} onStatus={notify}
    onEditor={() => openEditor(tab.workspace || workspace)} recoveringEditor={recoveringEditor}
    onRecover={() => recoverEditor(tab)} />;
  const dialogs = (
    <>
      {hostGitOpen && (
        <HostGitPreparation
          onClose={() => setStartOptions(null)}
          onReady={async () => {
            setStartRequest({ id: null });
            setStartOptions(null);
            const result = await action("/start", startOptions);
            setStartRequest(result ? { id: result.id } : null);
            if (result) refreshStatus.current?.();
          }}
        />
      )}
      {dialog && (
        <Modal
          title={dialog.title}
          anchor={dialog.anchor}
          onClose={dialog.pending ? undefined : () => finishDialog(false)}
          footer={
            <>
              <button disabled={dialog.pending} onClick={() => finishDialog(false)}>取消</button>
              {dialog.secondary && (
                <button disabled={dialog.pending} onClick={() => finishDialog("secondary")}>
                  {dialog.secondary}
                </button>
              )}
              <button className="primary" disabled={dialog.pending} onClick={() => finishDialog(true)}>
                {dialog.pending ? dialog.pendingLabel : dialog.confirm}
              </button>
            </>
          }
        >
          <div className="modal-body">
            <p>{dialog.body}</p>
            {dialog.error && <div className="error-message" role="alert">{dialog.error}</div>}
          </div>
        </Modal>
      )}
      {quitting && (
        <Modal
          title={quitting.error ? "未能完成退出" : "正在停止环境"}
          onClose={cancelAutomaticExit}
          footer={
            <>
              <button onClick={cancelAutomaticExit}>
                {quitting.error ? "留在 Cage" : "取消自动退出"}
              </button>
              {quitting.error && (
                <button className="primary" onClick={stopAndQuit}>
                  重试停止并退出
                </button>
              )}
            </>
          }
        >
          <div
            className="modal-body"
            role={quitting.error ? "alert" : "status"}
          >
            <p>
              {quitting.error ||
                (connectionError
                  ? "正在恢复工作台连接，恢复后会继续确认停止结果。"
                  : "停止完成后 Cage 会自动退出。取消自动退出只保留窗口，环境仍会停止。")}
            </p>
            {!quitting.error && (
              <p>
                <LoaderCircle size={16} className="spin" />{" "}
                {status?.job?.running
                  ? status.job.message
                  : "正在确认环境已停止…"}
              </p>
            )}
          </div>
        </Modal>
      )}
    </>
  );
  if (fatal)
    return (
      <>
        <div className="boot-screen">
          <Shield size={36} />
          <h1>连接 Cage 工作台</h1>
          <p>{fatal}</p>
          <p className="muted">
            请从 Cage 应用打开，或在终端运行 <code>cage desktop</code>。
          </p>
          <button
            className="primary"
            onClick={() =>
              nativeAvailable() ? native("reconnect") : location.reload()
            }
          >
            重新连接
          </button>
        </div>
        {dialogs}
      </>
    );
  if (!status || !settings)
    return (
      <>
        <div className="boot-screen">
          <div className="brand-mark">C</div>
          <h1>Cage</h1>
          <p>
            <LoaderCircle size={16} className="spin" />
            正在连接工作区…
          </p>
        </div>
        {dialogs}
      </>
    );
  return (
    <SafetyChecklistContext.Provider value={() => setSafetyOpen(true)}>
    <div className={`app ${sidebarCollapsed ? "sidebar-collapsed" : ""}`}>
      <div
        className="workbench-shell"
        inert={settingsOpen || hostGitOpen || undefined}
      >
        {!sidebarCollapsed && (
          <WorkspaceSidebar
            downloadAction={<BrowserDownloads sidebar downloads={downloads} ready={status.ready && !busy} onStatus={notify} />}
            updateAction={<AppUpdateButton updates={appUpdates} />}
            workspace={selectedProject || status.workspace}
            guestSystem={account?.guest_system || status.guest_system || guestSystem?.guest_system || "debian-13"}
            defaultWorkspace={status.environment_directory}
            projects={projects}
            tabs={displayTabs}
            sessions={cliSessions.sessions.map(session => ({ ...session,
              claudeAccountNumber: claudeNumbers.get(session.account_id) }))}
            sessionError={cliSessions.error}
            sessionRefreshing={cliSessions.loading}
            onSessionRefresh={cliSessions.refresh}
            onSessionOpen={openSessionTerminal}
            openingSession={openingSession}
            sessionOpenError={sessionOpenError}
            onSessionArchive={archiveSession}
            onSessionPin={pinSession}
            onSessionRename={session => setRename({id:session.id, title:session.title, session:true})}
            saved={metadata}
            active={active}
            home={showHome}
            busy={busy || storageRecovery}
            ready={status.ready}
            running={running}
            runtimeLabel={runtimeLabel}
            network={network}
            onHome={() => { setDesktopOpen(false); setHome(true); }}
            onOpenProject={(path = "") => setProjectDialog({ path })}
            onSelect={select}
            onClose={closeTab}
            onRename={renameTab}
            onPin={pinTab}
            splitDrag={splitDrag}
            onReorder={reorder}
            onSessionReorder={(path, order) => changeTabMetadata({ ...metadata,
              sessionOrder: { ...metadata.sessionOrder, [path]: order } })}
            onProjectSelect={setSelectedProject}
            onNewTerminal={newTerminal}
            terminalMenuAt={terminalMenuAt}
            onBrowser={openBrowser}
            onTailscale={() => setTailscaleOpen(true)}
            desktopOpen={desktopOpen}
            onClaudeWeb={() => openBrowserPage("https://claude.ai/")}
            onClaudeDesktop={() => { setHome(false); setDesktopOpen(true); }}
            onEditor={openEditor}
            onSettings={openSettings}
            onAccountConfirm={confirm}
            onAccountStatus={notify}
            onSafety={() => setSafetyOpen(true)}
            onCollapse={() =>
              persist("cage-sidebar-collapsed", true, setSidebarCollapsed)
            }
          />
        )}
        <div className="workbench-main">
          {showHome && terminalHeader(null)}
          {browserError && running && !connectionError && <div className="connection-banner" role="status">
            <span>客机浏览器暂时无法连接。{browserError}</span>
            <button onClick={() => refreshStatus.current?.()}>重试</button>
          </div>}
          {connectionError && (
            <div className="connection-banner" role="alert">
              工作台连接中断，后台状态暂时未知。
              <button
                onClick={() =>
                  nativeAvailable() ? native("reconnect") : location.reload()
                }
              >
                重新连接工作台
              </button>
            </div>
          )}
          <main className={`workspace ${split ? "workspace-split" : ""}`} ref={workspaceBox}
            style={{ "--split-left": `${100 * (dualBrowser && nativeSplit?.fraction || 0.5)}%` }}>
            {dropSide && <div aria-hidden="true" className={`split-drop split-${dropSide}`}>{dropSide === "left" ? "放到左侧" : "放到右侧"}</div>}
            {!storageRecovery && desktopOpen && macGuest && <MacOSClaudeWindow uploads={desktopFiles.uploads} keyboard={status.keyboard} remoteRef={remote}
              onStatus={notify} onFallback={(text, message) => setTransfer({ text, message })} visible={!settingsOpen} onConfirm={confirm}
              sidebarCollapsed={sidebarCollapsed} onExpand={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)} />}
            {!storageRecovery && desktopOpen && !macGuest && <ClaudeDesktop
              uploads={desktopFiles.uploads}
              onProjectOpened={() => refreshStatus.current?.()}
              status={status} visible={!settingsOpen} remoteRef={remote}
              onStatus={notify} onFallback={(text, message) => setTransfer({ text, message })}
              onConfirm={confirm} onDetails={() => setDetails(true)}
              sidebarCollapsed={sidebarCollapsed}
              onExpand={() => persist("cage-sidebar-collapsed", false, setSidebarCollapsed)}
            />}
            {status.environment_update_required && !busy && (
              <div className="environment-update-banner" role="status">
                <span>{status.environment_update_error || "环境组件需要更新。请先保存编辑器中的文件。"}</span>
                <button onClick={() => openSettings("project")}>查看并更新</button>
              </div>
            )}
            {!storageRecovery && (status.vm_running || terminal) && (
              <>
                {!desktopOpen && !showHome && webPanes.length > 0 && (
                  <section className={`pane-lane browser-lane ${browserTabLayout === "vertical" ? "vertical-browser" : ""} ${dualBrowser ? "" : "split-" + paneSide(webPanes[0].id)}`}>
                    {browserTabLayout === "vertical" && <BrowserTabRail tabs={browserTabs} metadata={metadata}
                      active={liveBrowser?.id} ready={status.ready} busy={busy} splitDrag={splitDrag}
                      sidebarButton={sidebarButton(paneSide(webPanes[0].id))}
                      onSelect={select} onClose={closeTab} onRename={renameTab} onPin={pinTab} onReorder={reorder}
                      onNew={newPage} />}
                    <div className="browser-headers">{webPanes.map(tab =>
                      <div key={paneSide(tab.id) || "single"} className="browser-header-cell" data-focused={active === tab.id}
                        onPointerDownCapture={() => focusHeader(tab)} onFocusCapture={() => focusHeader(tab)}>
                        {browserHeader(tab)}
                      </div>)}</div>
                    <div className="pane-body" onPointerDownCapture={() => {
                      if (!dualBrowser && liveBrowser?.id !== active) focusHeader(liveBrowser);
                    }}>
                      {status.ready && liveBrowser?.kind === "browser-settings" && <BrowserSettingsPage
                        key={`${liveBrowser.id}:${browserSettingsRevision}`} tab={liveBrowser} tabs={tabs}
                        currentHome={settings.browser_home_url} layout={browserTabLayout}
                        onLayoutChange={value => persist("cage-browser-tab-layout", value, setBrowserTabLayout)}
                        onSectionChange={openBrowserSettings}
                        onSaved={next => setSettings(previous => ({ ...previous, ...next }))}
                        onInternal={openInternalPage} onOpenPage={openSettingsPageResult} onConfirm={confirm} />}
                      {status.ready && <RemoteView ref={remote} visible={Boolean(liveBrowserVisible && !settingsOpen && liveBrowser?.kind !== "browser-settings")}
                        copyScope={webPanes.map(tab => `${tab.id}:${tab.document || ""}`).join("|")}
                        focused={dualBrowser || liveBrowser?.id === active} keyboard={status.keyboard}
                        guestPlatform={status.guest_system} systemClipboard={browser.capabilities?.systemClipboard !== false}
                        tabId={liveBrowser?.id} onStatus={notify}
                        onFallback={(text, message) => setTransfer({ text, message })} />}
                      {!status.ready && <div className="view-overlay">
                        <RefreshCw /><h3>浏览器连接暂时不可用</h3>
                        <p>环境和终端仍可运行；恢复浏览器不会停止现有终端。</p>
                        <button className="primary" disabled={busy} onClick={() => setStartOptions({})}>恢复浏览器</button>
                      </div>}
                      {status.ready && !splitReady && <div className="view-overlay">正在调整浏览器布局…</div>}
                    </div>
                  </section>
                )}
                {tabs.filter(t => t.kind === "terminal").map(tab => {
                  const visible = !desktopOpen && !showHome && panes.some(pane => pane.id === tab.id);
                  return <section key={tab.id} className={`pane-lane terminal-lane split-${paneSide(tab.id)}`}
                    data-focused={active === tab.id} hidden={!visible}
                    onPointerDownCapture={() => focusHeader(tab)} onFocusCapture={() => focusHeader(tab)}>
                    {visible && terminalHeader(tab)}
                    <div className="pane-body">
                      <TerminalView
                        session={{ ...tab, id: tab.sessionId }}
                        onLoginTab={({ id }) => showBrowserResult({ id, kind: "web", login: true, incognito: true, title: "账号授权" })}
                        onControls={(control) =>
                          control
                            ? terminalControls.current.set(tab.id, control)
                            : terminalControls.current.delete(tab.id)
                        }
                        visible={visible && !settingsOpen}
                        focused={active === tab.id}
                        onStatus={notify}
                        onAccountChanged={view => { if (view?.desktop_terminal_id && view.desktop_terminal_id !== tab.sessionId) void openSessionTerminal({id:tab.cli_session_id}); }}
                      />
                    </div>
                  </section>;
                })}
              </>
            )}
            {showHome && (
              <div className="welcome">
                <div className="welcome-content">
                  <img className="welcome-logo" src={logo} alt="Cage" />
                  {storageRecovery ? <>
                    <h1>环境存储需要恢复</h1>
                    <p className="welcome-subtitle"><SafetyMessage message={status.storage_recovery.error} /></p>
                    <p className="field-help">在设置中恢复环境存储后，再启动浏览器和终端。诊断包可以从同一页面导出。</p>
                    <button className="primary" onClick={() => openSettings("project")}>恢复环境存储</button>
                  </> : setupProgress ? <>
                    <h1>{status.job.error ? "环境安装未完成" : "正在安装 Cage 环境"}</h1>
                    <SetupProgress job={status.job} onDetails={() => setDetails(true)}
                      onStop={stop} onRetry={() => setStartOptions(status.job.kind === "guest-install" ? { guest_system: status.job.setup_progress.system } : {})} disconnected={Boolean(connectionError)} />
                  </> : <>
                  <h1>
                    {busy
                      ? status.job.kind === "stop"
                        ? "正在停止 Cage 环境"
                        : status.job.kind === "storage"
                          ? "正在调整环境存储"
                        : status.job.kind === "project"
                          ? "正在打开项目"
                          : "正在准备 Cage 环境"
                      : running
                        ? !status.ready
                          ? "Cage 环境正在连接"
                          : "Cage 环境已就绪"
                        : setupProxy
                          ? "为 Claude 准备独立工作区"
                          : "先启动 Cage 环境"}
                  </h1>
                  <p className="welcome-subtitle">
                    <SafetyMessage message={busy
                      ? status.job.message
                      : running
                        ? !status.ready
                          ? status.environment_update_error || "浏览器连接尚未就绪，可以恢复连接或继续使用终端。"
                          : network.mode === "open"
                            ? "环境已连接，网络检查通过。可以浏览网页或打开本地项目。"
                            : `${network.label}。终端与本地文件仍可使用。`
                        : setupProxy
                          ? "先连接你的代理，再启动浏览器、终端和编辑器。无需安装额外的代理软件。"
                          : "准备独立的浏览器和终端，完成网络检查后开始工作。"} />
                  </p>
                  {!running && !nativeGuestLabel && (
                    <ol className="onboarding-steps" aria-label="开始使用 Cage">
                      {["配置代理", "启动环境", "开始工作"].map(
                        (label, index) => (
                          <li
                            key={label}
                            aria-current={
                              index === (setupProxy ? 0 : 1)
                                ? "step"
                                : undefined
                            }
                          >
                            <span>{index + 1}</span>
                            {label}
                          </li>
                        ),
                      )}
                    </ol>
                  )}
                  <div className="start-project-box">
                    <div className="environment-summary">
                      <EnvironmentShield
                        healthy={status.ready && network.mode === "open"}
                        size={24}
                      />
                      <span>
                        <strong>Cage 隔离环境</strong>
                        <small>
                          {running
                            ? runtimeLabel
                            : setupProxy
                              ? "自备代理订阅、YAML 配置或节点"
                              : nativeGuestLabel ? `${nativeGuestLabel} 客机已安装` : "代理已配置，首次启动会下载开发环境"}
                        </small>
                      </span>
                    </div>
                    {!busy && !running && !account?.guest_system && status.local_vm_storage && !isRemoteClient() && <GuestSystemPicker system={guestSystem} onChange={setGuestSystem} />}
                    {!running && !nativeGuestLabel && status.local_vm_storage && !isRemoteClient() && <button className="storage-entry text-button" disabled={busy} onClick={() => openSettings("project")}>选择环境存储位置</button>}
                    <div className="start-project-footer">
                      <span>
                        <i className={`status-dot ${network.mode}`} />
                        {busy
                          ? runtimeLabel
                          : running
                            ? network.label
                            : setupProxy
                              ? "等待配置代理"
                              : "环境已停止"}
                      </span>
                      <button
                        className="primary"
                        data-terminal-location="start"
                        disabled={busy || Boolean(connectionError) || (!running && !setupProxy &&
                          ((!account?.guest_system && status.local_vm_storage && !isRemoteClient() && guestSystem === undefined) || !validGuestChoice(guestSystem)))}
                        onClick={(event) => {
                          if (!running && guestSystem?.activate) {
                            api("/environment/guest-systems/activate", {}).then(() => native("reconnect")).catch((e) => setToast(e.message));
                          } else if (!running && setupProxy) openSettings("network");
                          else if (!running || !status.ready)
                            setStartOptions(!running && guestSystem?.guest_system === "debian-13" && !guestSystem.clone ? { guest_system: "debian-13" } : !running && guestSystem ? {
                              guest_system: guestSystem.guest_system, media: guestSystem.media, sha256: guestSystem.sha256,
                              clone_key: guestSystem.clone_key,
                              editor_terms: guestSystem.editor_terms,
                            } : {});
                          else if (status.environment_workspace)
                            setProjectDialog({ path: "" });
                          else newTerminal(workspace, event.currentTarget);
                        }}
                      >
                        {busy ? (
                          <LoaderCircle size={16} className="spin" />
                        ) : !running && setupProxy ? (
                          <Globe2 size={16} />
                        ) : !running ? (
                          <Power size={16} />
                        ) : !status.ready ? (
                          <RefreshCw size={16} />
                        ) : status.environment_workspace ? (
                          <FolderOpen size={16} />
                        ) : (
                          <TerminalSquare size={16} />
                        )}
                        {busy
                          ? "正在处理…"
                          : !running
                            ? setupProxy
                              ? "配置网络与代理"
                              : guestSystem?.activate ? "打开工作台" : guestSystem?.pending ? "继续安装" :
                                guestSystem && guestSystem.guest_system !== "debian-13" ? "安装客户机" : "启动 Cage 环境"
                            : !status.ready
                              ? "恢复浏览器"
                              : status.environment_workspace
                                ? "打开项目"
                                : "新建终端"}
                      </button>
                    </div>
                    {terminalMenuAt("start")}
                  </div>
                  {running && (
                    <div className="welcome-shortcuts">
                      {macGuest && <MacOSLogin disabled={!status.ready || busy} />}
                      <button onClick={newTerminalHere} data-terminal-location="shortcuts" aria-haspopup="menu" disabled={busy}>
                        <TerminalSquare size={16} />
                        新建终端
                      </button>
                      <button
                        onClick={() => openEditor(contentWorkspace)}
                        disabled={!status.ready || busy}
                      >
                        <FileCode2 size={16} />
                        浏览文件
                      </button>
                    </div>
                  )}
                  {terminalMenuAt("shortcuts")}
                  {!running && !nativeGuestLabel && (
                    <p className="welcome-hint">
                      {setupProxy
                        ? "Cage 不提供代理或 Claude 账户。连接后，在 Cage 内完成 Claude 登录。"
                        : "首次安装时可在操作详情中查看下载和配置进度；中断后可以继续安装。"}
                    </p>
                  )}
                  {running && status.ready && (
                    <p className="welcome-hint">
                      打开项目后，在终端输入 <code>claude</code>
                      ，按提示登录你的账户。
                      <br />
                      {isRemoteClient() ? "默认工作区和打开的项目位于云端，编辑和删除会直接影响云端文件。" : "默认工作区和打开的项目与本机共享，编辑和删除文件会同步。"}
                    </p>
                  )}
                  </>}
                  {status.project_error && (
                    <div className="start-error" role="alert">
                      <strong>部分项目文件夹尚未连接</strong>
                      <p>环境和浏览器可继续使用。</p>
                      {status.project_errors?.length ? (
                        <ul className="project-connection-errors">
                          {status.project_errors.map(({ path, message, recovery }) => (
                            <li key={path}>
                              <strong>{projectName(path, status.environment_directory)}</strong>
                              <span className="project-error-path">{path}</span>
                              <p>{message}</p>
                              <ProjectRecovery recovery={recovery} disabled={!status.ready || busy}
                                onRetry={() => setProjectDialog({ path })} onSettings={() => openSettings("project")} />
                            </li>
                          ))}
                        </ul>
                      ) : <p>{status.project_error}</p>}
                    </div>
                  )}
                  {!storageRecovery && !setupProgress && status.job?.error && (
                    <div className="start-error" role="alert">
                      <strong>{status.job.message || "本次操作未完成"}</strong>
                      <p><SafetyMessage message={status.job.error} /></p>
                      {status.can_restore && (
                        <button
                          className="text-button"
                          onClick={() => openSettings("project")}
                        >
                          查看恢复选项
                        </button>
                      )}
                    </div>
                  )}
                  {!setupProgress && status.job?.log && (
                    <button
                      className="text-button details-link"
                      onClick={() => setDetails(true)}
                    >
                      查看操作详情
                    </button>
                  )}
                </div>
              </div>
            )}
          </main>
          <footer className="status-bar">
            {sidebarCollapsed && <AppUpdateButton updates={appUpdates} />}
            <button onClick={() => setSafetyOpen(true)} title="网络与隔离检查">
              <EnvironmentShield
                size={14}
                healthy={status.ready && network.mode === "open"}
              />
              <span>{running ? network.label : "网络检查"}</span>
            </button>
            <span className="status-divider" />
            <button onClick={() => openSettings("network")}>
              <MapPin size={12} />
              <span>
                {status.network_error
                  ? "出口待确认"
                  : status.location || status.safety?.exit_ip
                    ? [
                        status.safety?.exit_ip || status.location?.ip,
                        status.location?.country,
                        status.location?.city,
                      ].filter(Boolean).join(" · ")
                    : "指定代理出口"}
              </span>
            </button>
            <span className="status-divider" />
            <button onClick={() => openSettings("region")}>
              <Clock3 size={12} />
              {settings.timezone}
              {settings.auto_timezone ? " · 自动" : ""}
            </button>
            <span className="status-divider" />
            <NetworkLatency
              key={`${proxy?.selected}:${proxy?.revision}:${running}:${busy}:${Boolean(connectionError)}`}
              enabled={running && !busy && !connectionError}
              revision={proxy?.revision}
              selected={proxy?.selected}
            />
            <span className="status-spacer" />
            <AudioControls audio={audio} />
            <span className="status-feedback" role="status">
              <SafetyMessage message={toast ||
                (busy
                  ? status.job.message
                  : running
                    ? status.ready
                      ? isRemoteClient() ? "文件保存在云端" : "文件与本机同步"
                      : "环境运行中 · 浏览器待恢复"
                    : "")} />
            </span>
            {status.job?.log && (
              <button title="操作详情" onClick={() => setDetails(true)}>
                <MoreHorizontal size={15} />
              </button>
            )}
            <button
              onClick={stop}
              disabled={
                Boolean(startRequest) || (!running && !busy) || (status?.job?.kind === "stop" && busy)
              }
              title="停止当前环境"
            >
              <Power size={12} />
              <span>停止环境</span>
            </button>
          </footer>
        </div>
      </div>
      <BrowserUploads requests={status.ready ? browser.uploads : []} onStatus={notify} />
      {settingsOpen && (
        <SettingsPanel
          current={settings}
          status={status}
          initialSection={settingsSection}
          claudeProfiles={claudeProfiles}
          terminalTabs={tabs}
          onClaudeLogin={(id) => createTerminal(workspace, "claude-login", id)}
          onClose={() => setSettingsOpen(false)}
          onSaved={refreshSettings}
          onConfirm={confirm}
        />
      )}
      {tailscaleOpen && <TailscalePanel status={status} onClose={() => setTailscaleOpen(false)}
        onConfirm={confirm} onInstall={async () => {
          const nativeGuest = ["macos", "windows-11"].includes(status.guest_system);
          if (nativeGuest && !await confirm({ title: "更新环境组件？", body: "更新会重新连接浏览器和显示组件，请先保存工作。设备身份、公钥和个人数据保留。", confirm: "更新组件" })) return;
          await action(nativeGuest ? "/environment/update" : "/tailscale/install", {});
        }}
        onOpenBrowser={async url => { const result = await openBrowserPage(url, false); if (result) setTailscaleOpen(false); return Boolean(result); }} />}
      {projectDialog && (
        <ProjectDialog
          current={settings}
          status={status}
          initialPath={projectDialog.path}
          onClose={() => setProjectDialog(null)}
          onSettings={() => { setProjectDialog(null); openSettings("project"); }}
          onOpened={openedProject}
          onRemoved={async (removed, workspace) => {
            await refreshSettings();
            await refreshStatus.current?.();
            if (selectedProject === removed) setSelectedProject(workspace);
          }}
        />
      )}
      {rename && (
        <Modal
          title="重命名标签"
          onClose={() => setRename(null)}
          footer={
            <>
              <span className="save-note">仅修改工作台中显示的名称</span>
              <button onClick={() => setRename(null)}>取消</button>
              <button
                className="primary"
                form="rename-tab"
                type="submit"
                disabled={!rename.title.trim()}
              >
                保存名称
              </button>
            </>
          }
        >
          <form
            className="modal-body project-form"
            id="rename-tab"
            onSubmit={async (event) => {
              event.preventDefault();
              if (!rename.title.trim()) return;
              if (rename.session) {
                try {
                  await api("/cli/sessions/" + rename.id + "/metadata", {title:rename.title.trim()});
                  sessionsChanged(); setRename(null);
                } catch (error) { notify(error.message); }
                return;
              }
              const tab = tabs.find(tab => tab.id === rename.id);
              if (tab?.cli_session_id) {
                try {
                  await updateSharedMetadata(tab, {title:rename.title.trim()});
                  setRename(null);
                } catch (error) { notify(error.message); }
                return;
              }
              changeTabMetadata({
                ...metadata,
                [rename.id]: {
                  ...metadata[rename.id],
                  title: rename.title.trim(),
                },
              });
              setRename(null);
            }}
          >
            <label htmlFor="tab-title">标签名称</label>
            <input
              id="tab-title"
              autoFocus
              maxLength={100}
              value={rename.title}
              onChange={(event) =>
                setRename({ ...rename, title: event.target.value })
              }
            />
          </form>
        </Modal>
      )}
      {transfer && (
        <Modal
          title="文本传递"
          onClose={() => setTransfer(null)}
          footer={
            <>
              <span className="save-note">仅在你的操作下传递文本</span>
              <button onClick={() => setTransfer(null)}>关闭</button>
              <button
                className="primary"
                onClick={() => {
                  remote.current?.pasteText(transfer.text);
                  setTransfer(null);
                }}
              >
                粘贴到 Cage
              </button>
            </>
          }
        >
          <div className="modal-body">
            <p>{transfer.message}</p>
            <textarea
              aria-label="待传递文本"
              className="transfer-text"
              value={transfer.text}
              onChange={(e) =>
                setTransfer({ ...transfer, text: e.target.value })
              }
            />
          </div>
        </Modal>
      )}
      {details && (
        <Modal title="操作详情" wide onClose={() => setDetails(false)}>
          <div className="modal-body">
            <p><SafetyMessage message={status.job.message} /></p>
            {status.job.error && (
              <div className="error-message"><SafetyMessage message={status.job.error} /></div>
            )}
            <pre className="job-log">{status.job.log || "暂无日志"}</pre>
          </div>
        </Modal>
      )}
      {safetyOpen && (
        <SafetyPanel
          status={
            connectionError
              ? {
                  ...status,
                  safety: {
                    ...status.safety,
                    gate: null,
                    error: "控制服务连接中断，无法确认当前外网状态",
                  },
                }
              : status
          }
          onClose={() => setSafetyOpen(false)}
          onSettings={openSettings}
          onOpenPage={openBrowserPage}
          onConfirm={confirm}
        />
      )}
      {dialogs}
    </div>
    </SafetyChecklistContext.Provider>
  );
}
export default function Workbench() {
  return <AccountProvider><AppearanceProvider><AccountGate><InitializationGate><App /></InitializationGate></AccountGate></AppearanceProvider></AccountProvider>;
}
