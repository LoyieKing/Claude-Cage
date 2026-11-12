import { isMac, terminalModifier } from "./platform.mjs";
import React, {
  useEffect,
  useRef,
  useState,
  useImperativeHandle,
  forwardRef,
} from "react";
import { useAppearance } from "./appearance";
import { terminalTheme, systemCodeFont } from "./appearance-state.mjs";
import { Terminal } from "@xterm/xterm";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { claudeLoginLink, isClaudeLoginLink } from "./terminal-login-link.mjs";
import { FitAddon } from "@xterm/addon-fit";
import RFB from "../.generated/novnc/core/rfb.js";
import { api, writeClipboard, nativeAvailable } from "./api";
import { runtimeSocketURL } from "./client-context.mjs";
import { readClipboardContent, transferFiles, hasFiles, imagePayload, prepareClipboardPaste, sendImage } from "./image-transfer";
import { terminalClipboardText } from "./terminal-clipboard.mjs";
import { terminalGapNote, terminalInputID, terminalInputStatusNote } from "./terminal-recovery.mjs";
import { sendFiles, shellFilePaths } from "./file-transfer";
import { installRemoteInput } from "./remote-input";
import { useWindowVisibility } from "./window-visibility";
import { displayGeometry, displaySizeSync, watchPixelRatio } from "./remote-display.mjs";
import { installLocalCursorFallback } from "./remote-cursor.mjs";
import { RefreshCw, Clipboard, ArrowUpRight } from "lucide-react";
import { SessionAccount } from "./session-account";
import "@xterm/xterm/css/xterm.css";

const terminalAccountRequest = (path, options) => api("/cli" + path, options.body);

export const RemoteView = forwardRef(function RemoteView(
  { visible, focused = true, keyboard, guestPlatform = "linux", systemClipboard = true, tabId, copyScope = tabId, onStatus, onFallback, desktop = false, zoom = 1 },
  ref,
) {
  const windowVisible = useWindowVisibility();
  const screenVisible = visible && windowVisible;
  const box = useRef(),
    rfb = useRef(),
    input = useRef(),
    current = useRef({ visible, keyboard, onStatus, onFallback }),
    displayResize = useRef();
  current.current = { visible: screenVisible && focused, keyboard, guestPlatform, systemClipboard, tabId, onStatus, onFallback, zoom };
  const failures = useRef(0);
  const [connection, setConnection] = useState("connecting"),
    [attempt, setAttempt] = useState(0),
    [sizeError, setSizeError] = useState(false);
  useEffect(() => { displayResize.current?.update(); }, [zoom]);
  useEffect(() => { input.current?.cancelCopy(); }, [copyScope]);
  useImperativeHandle(ref, () => ({
    focus: () => rfb.current?.focus(),
    edit: (action) =>
      input.current?.focused()
        ? input.current?.edit(action)
        : false,
    copy: () => input.current?.copy(),
    receiveCopy: (value) => input.current?.receiveCopy(value),
    paste: () => input.current?.readAndPaste(),
    pasteText: (text) => input.current?.paste(text),
    transfer: () =>
      onFallback(input.current?.text() || "", "可在此使用本机复制、粘贴。"),
  }));
  useEffect(() => {
    if (!screenVisible) return;
    const container = box.current;
    if (!container) return;
    let disposed = false,
      client,
      resizeObserver,
      unwatchDensity,
      sizeSync,
      reconnectTimer;
    setConnection("connecting");
    setSizeError(false);
    const retry = () => {
      if (disposed) return;
      setConnection("disconnected");
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(
        () => setAttempt((v) => v + 1),
        Math.min(1000 * 2 ** failures.current++, 5000),
      );
    };
    const desktopPrefix = desktop ? "/claude-desktop" : "";
    api(`${desktopPrefix}/vnc-credentials`)
      .then(({ password, display_scale, dynamic_scale }) => {
        if (disposed) return;
        // Such a guest renders at the density of the screen showing it.
        let followScreen = dynamic_scale === true;
        client = new RFB(
          container,
          runtimeSocketURL(`${desktopPrefix}/vnc`),
          { credentials: { password }, cageBrowserDisplay: !desktop, cageCursorFallback: desktop && guestPlatform === "windows" },
        );
        rfb.current = client;
        installLocalCursorFallback(client._cursor,
          !desktop && ["darwin", "macos"].includes(guestPlatform));
        client.scaleViewport = true;
        client.resizeSession = false;
        client.background = "#ffffff";
        client.qualityLevel = 8;
        client.compressionLevel = 2;
        client.addEventListener("connect", () => {
          if (disposed) return;
          failures.current = 0;
          setConnection("connected");
          sizeSync = displaySizeSync(
            async (size) => {
              const reply = await api(desktop ? "/claude-desktop/resize" : "/browser/resize", size);
              // The guest kept its own density: size the display for that one.
              if (size.scale !== undefined && reply?.scale !== size.scale && followScreen) {
                followScreen = false;
                resize();
              }
            },
            { onError: () => setSizeError(true), onSuccess: () => setSizeError(false) },
          );
          const resize = () => {
            if (disposed) return;
            sizeSync.update(displayGeometry(container.getBoundingClientRect(), {
              desktop, zoom: current.current.zoom, density: display_scale,
              pixelRatio: followScreen ? window.devicePixelRatio : undefined,
            }));
          };
          displayResize.current = { update: resize, retry: () => sizeSync.retry() };
          resizeObserver = new ResizeObserver(resize);
          resizeObserver.observe(container);
          unwatchDensity = watchPixelRatio(resize);
          resize();
          const preparePaste = () => {
            const id = current.current.tabId;
            return prepareClipboardPaste(desktop ? "desktop" : "browser", id, {
              valid: () => !disposed && current.current.visible && current.current.tabId === id,
            });
          };
          input.current = installRemoteInput({
            events: client,
            setKeyboardTarget: target => client.cageSetKeyboardTarget(target),
            sendKey: (...key) => client.sendKey(...key),
            focus: () => client.focus(),
            pasteText: text => {
              if (desktop || current.current.systemClipboard) return client.cagePasteText(text);
              const prepared = preparePaste();
              return client.cageRunInput(() => prepared.send({ text })).finally(() => prepared.dispose());
            },
            runKeyAction: action => client.cageRunInput(action),
          }, container, {
            guestPlatform: () => current.current.guestPlatform,
            systemClipboard: () => current.current.systemClipboard,
            copyText: !desktop
              ? (_sendCopy, valid, { id, context, key }) => {
                if (current.current.systemClipboard !== false) return undefined;
                if (!valid()) throw new Error("复制位置已切换，请重新复制");
                return api("/browser/" + (key === "x" ? "cut" : "copy"), { id, context });
              }
              : undefined,
            setCopyContext: desktop ? undefined : (context, valid) => client.cageRunInput(async () => {
              if (!valid()) return;
              try { await api("/browser/copy-context", { id: current.current.tabId, context }); }
              catch {
                if (valid()) {
                  input.current?.cancelCopy();
                  current.current.onStatus("复制按钮同步暂未就绪，可使用复制快捷键或文本传递");
                }
                // A metadata failure must not cancel the user's queued click.
              }
            }),
            preparePaste,
            pasteContent: (content, prepared) => {
              const id = current.current.tabId;
              const valid = () => !disposed && current.current.visible && current.current.tabId === id;
              return client.cagePasteContent(content,
                (payload) => {
                  if (!valid()) throw new Error("输入目标已切换，请重新粘贴图片");
                  return payload.files || payload.fileSelection
                    ? sendFiles(payload, desktop ? "desktop" : "browser", id, { valid })
                    : sendImage(payload, desktop ? "desktop" : "browser", id, { prepared });
                }, valid, !desktop && !current.current.systemClipboard ? text => prepared.send({ text }) : undefined);
            },
            insertText: (text) => {
              if (desktop) {
                if (!window.cageSharedClipboardActive) return client.cagePasteText(text);
                return client.cageRunInput(async () => {
                  const valid = () => !disposed && current.current.visible;
                  const state = await api("/input", { target: "desktop", id: "desktop", mode: "status" });
                  if (!state.available || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
                  const session = crypto.randomUUID().replaceAll("-", "");
                  const begun = await api("/input", { target: "desktop", id: "desktop", mode: "begin", context: state.context, session, sequence: 0 });
                  if (!begun.available || !valid()) throw new Error("输入位置已失去焦点，请重新点击输入框");
                  const result = await api("/input", { target: "desktop", id: "desktop", mode: "commit", session, sequence: 1, text });
                  if (!result.available) throw new Error("文字输入未完成，请重新点击输入框");
                });
              }
              const id = current.current.tabId;
              return client.cageRunInput(() => {
                if (disposed || !current.current.visible || current.current.tabId !== id)
                  throw new Error("输入位置已切换，请重新输入");
                return api("/browser/insert-text", { id, text });
              });
            },
            insertComposition: (id, value) => client.cageRunInput(async () => {
              if (value.mode !== "cancel" && (disposed || !current.current.visible ||
                  (desktop ? "desktop" : current.current.tabId) !== id))
                throw new Error("输入位置已切换，请重新输入");
              const result = await api("/input", { target: desktop ? "desktop" : "browser", id, ...value });
              if (value.mode !== "cancel" && !result.available)
                throw new Error("输入位置已失去焦点，请重新输入");
              return result;
            }),
            tabId: () => desktop ? "desktop" : current.current.tabId,
            caretRect: async (session) => {
              const id = desktop ? "desktop" : current.current.tabId;
              const value = await api("/input", { target: desktop ? "desktop" : "browser", id, mode: "status", session });
              return !disposed && current.current.visible &&
                (desktop ? "desktop" : current.current.tabId) === id ? value : null;
            },
            mode: () => current.current.keyboard,
            isActive: () => current.current.visible,
            onStatus: (t) => current.current.onStatus(t),
            onFallback: (...args) => current.current.onFallback(...args),
          });
          if (current.current.visible && !document.activeElement?.closest(".chrome")) client.focus();
        });
        client.addEventListener("disconnect", () => {
          if (disposed) return;
          input.current?.dispose();
          input.current = null;
          sizeSync?.dispose();
          resizeObserver?.disconnect();
          unwatchDensity?.();
          displayResize.current = null;
          retry();
        });
        client.addEventListener("securityfailure", () => {
          retry();
        });
      })
      .catch(() => {
        retry();
      });
    return () => {
      disposed = true;
      sizeSync?.dispose();
      displayResize.current = null;
      clearTimeout(reconnectTimer);
      resizeObserver?.disconnect();
      unwatchDensity?.();
      input.current?.dispose();
      input.current = null;
      client?.disconnect();
      rfb.current = null;
    };
  }, [attempt, screenVisible, desktop]);
  useEffect(() => {
    if (focused && screenVisible && connection === "connected") {
      const t = setTimeout(() => { if (!document.activeElement?.closest(".chrome")) rfb.current?.focus(); }, 80);
      return () => clearTimeout(t);
    }
  }, [focused, screenVisible, connection]);
  return (
    <div className={`remote-view ${visible ? "" : "concealed"}`}>
      <div className="remote-canvas" ref={box} />
      {sizeError && connection === "connected" && (
        <div className="remote-size-warning connection-banner" role="alert">
          画面尺寸未能同步，显示比例可能不准确。
          <button onClick={() => displayResize.current?.retry()}>重试调整</button>
        </div>
      )}
      {connection !== "connected" && (
        <div className="view-overlay">
          <RefreshCw className={connection === "connecting" ? "spin" : ""} />
          <h3>
            {connection === "connecting" ? "正在连接画面" : "画面连接已断开"}
          </h3>
          <p>后台任务不会因画面断开而自动结束。</p>
          {connection === "disconnected" && (
            <button
              className="primary"
              onClick={() => setAttempt((v) => v + 1)}
            >
              重新连接画面
            </button>
          )}
        </div>
      )}
    </div>
  );
});

export function TerminalView({ session, visible, focused = true, onStatus, onControls, onAccountChanged, onLoginTab }) {
  const { appearance, theme, variant } = useAppearance();
  const box = useRef(),
    instance = useRef(),
    fit = useRef(),
    socket = useRef();
  const [connected, setConnected] = useState(false),
    [attempt, setAttempt] = useState(0),
    [preparing, setPreparing] = useState(Boolean(session.cli_session_id && !session.closed)),
    [gapNote, setGapNote] = useState("");
  const revealStartup = useRef();
  const visibleRef = useRef(visible);
  visibleRef.current = visible;
  const focusedRef = useRef(focused);
  focusedRef.current = focused;
  useEffect(() => {
    let disposed = false, emittingPaste = false, inputChain = Promise.resolve();
    const enqueue = (action) => {
      const result = inputChain.then(action);
      inputChain = result;
      const reset = () => { if (inputChain === result) inputChain = Promise.resolve(); };
      result.then(reset, reset);
      return result;
    };
    const activateLogin = async (_event, uri) => {
      if (disposed || !visibleRef.current || !focusedRef.current || !isClaudeLoginLink(uri)) return;
      try {
        const tab = await api(`/terminals/${session.id}/login-link`, { url: uri });
        if (!disposed && visibleRef.current && focusedRef.current) onLoginTab?.(tab);
      } catch (error) { if (!disposed) onStatus(error.message); }
    };
    const terminal = new Terminal({
      linkHandler: { activate(event, uri) {
        if (isClaudeLoginLink(uri)) return void activateLogin(event, uri);
        // Keep xterm's user confirmation for other explicit HTTP(S) links.
        if (window.confirm(`打开此终端链接？\n${uri}`)) window.open(uri, "_blank", "noopener,noreferrer");
      } },
      fontFamily: theme.fonts.code || systemCodeFont,
      fontSize: appearance.codeFontSize,
      lineHeight: 1.3,
      cursorBlink: true,
      macOptionClickForcesSelection: true,
      minimumContrastRatio: 4.5,
      scrollback: 6000,
      theme: terminalTheme(theme, variant),
    });
    const fitter = new FitAddon();
    terminal.loadAddon(fitter);
    terminal.loadAddon(new WebLinksAddon(activateLogin, { urlRegex: claudeLoginLink }));
    terminal.open(box.current);
    instance.current = terminal;
    fit.current = fitter;
    const ws = new WebSocket(
      runtimeSocketURL(`/terminals/${session.id}/stream`),
    );
    ws.binaryType = "arraybuffer";
    socket.current = ws;
    let startupPending = Boolean(session.cli_session_id && !session.closed);
    let startupVisible = !startupPending;
    setPreparing(startupPending);
    const reveal = () => {
      if (disposed || startupVisible) return;
      startupVisible = true;
      clearTimeout(startupTimer);
      setPreparing(false);
      if (visibleRef.current && focusedRef.current) terminal.focus();
    };
    revealStartup.current = reveal;
    // A stalled launch or login prompt must remain accessible. The output is
    // retained in xterm throughout preparation, including on reconnect.
    const startupTimer = startupPending ? setTimeout(reveal, 8000) : null;
    const painted = () => {
      if (disposed || startupPending || startupVisible) return;
      const buffer = terminal.buffer.active;
      const screen = Array.from({ length: terminal.rows }, (_, row) =>
        buffer.getLine(buffer.viewportY + row)?.translateToString(true) || "",
      ).join("").replace(/\s/g, "");
      // Channel registration can precede the TUI's final repaint. Reveal only
      // after the confirmation has left the visible screen.
      if (!screen.includes("WARNING:Loadingdevelopmentchannels")) reveal();
    };
    const startupRender = terminal.onRender(painted);
    const resize = () => {
      if (!visibleRef.current) return;
      try {
        fitter.fit();
        if (ws.readyState === WebSocket.OPEN)
          ws.send(
            JSON.stringify({
              type: "resize",
              cols: terminal.cols,
              rows: terminal.rows,
            }),
          );
      } catch {}
    };
    ws.onopen = () => {
      setConnected(true);
      resize();
      if (startupVisible && visibleRef.current && focusedRef.current) terminal.focus();
    };
    let historyFrame = false, replayingHistory = false;
    const clipboardHandler = terminal.parser.registerOscHandler(52, async (data) => {
      if (disposed || replayingHistory || !visibleRef.current || !focusedRef.current || !box.current?.contains(document.activeElement)) return true;
      const text = terminalClipboardText(data);
      if (text === undefined) return true;
      try {
        await writeClipboard(text);
        if (!disposed) onStatus("已复制终端选中文本");
      } catch { if (!disposed) onStatus("无法写入本机剪贴板"); }
      return true;
    });
    ws.onmessage = (event) => {
      if (typeof event.data === "string") {
        let state;
        try { state = JSON.parse(event.data); } catch { return; }
        if (state.type === "cli-startup") {
          startupPending = state.pending;
          terminal.refresh(0, terminal.rows - 1);
        }
        if (state.type === "history") historyFrame = true;
        if (state.type === "gap" || state.type === "lost") {
          const note = terminalGapNote(state);
          if (note) {
            setGapNote(note);
            terminal.write(`\r\n\x1b[33m[${note}]\x1b[0m\r\n`);
          }
          return;
        }
        if (state.type === "input-status") {
          const note = terminalInputStatusNote(state.status);
          if (note) setGapNote(note);
          return;
        }
        return;
      }
      if (historyFrame) {
        historyFrame = false;
        replayingHistory = true;
        terminal.write(new Uint8Array(event.data), () => { replayingHistory = false; });
      } else terminal.write(new Uint8Array(event.data));
    };
    ws.onclose = () => { setConnected(false); reveal(); };
    const send = (data) => {
      if (disposed || ws.readyState !== WebSocket.OPEN) throw new Error("终端已断开，请重新连接后重试");
      ws.send(JSON.stringify({ type: "input", id: terminalInputID(), data }));
    };
    terminal.onData((data) => {
      if (emittingPaste) send(data);
      else void enqueue(() => send(data)).catch(() => {});
    });
    const receive = (content) => {
      // Attach rejection immediately, even while another image is in flight.
      const ready = Promise.resolve(content).then(value => ({ value }), error => ({ error }));
      onStatus("正在传递剪贴板内容…");
      void enqueue(async () => {
        const { value, error } = await ready;
        if (error) throw error;
        if (disposed || !visibleRef.current || !focusedRef.current || ws.readyState !== WebSocket.OPEN) throw new Error("终端已切换或断开，请重新粘贴");
        let text = value.text || "";
        if (value.files || value.fileSelection) {
          const result = await sendFiles(value, "terminal", session.id, { valid: () => !disposed && visibleRef.current && focusedRef.current && ws.readyState === WebSocket.OPEN });
          text = shellFilePaths(result.paths);
        }
        if (value.png) {
          const result = await sendImage(value, "terminal", session.id, { valid: () => !disposed && visibleRef.current && focusedRef.current && ws.readyState === WebSocket.OPEN });
          text = shellFilePaths([result.path]);
        }
        if (disposed || !visibleRef.current || !focusedRef.current) throw new Error("终端已切换，请回到原终端重新粘贴");
        // Honor xterm's bracketed-paste mode without enqueuing behind this job.
        emittingPaste = true;
        try { terminal.paste(text); } finally { emittingPaste = false; }
        onStatus(value.png || value.files || value.fileSelection ? "附件已保存到 Cage，路径已插入终端" : "已粘贴");
      }).catch((error) => { if (!disposed) onStatus(error.message); });
    };
    const host = box.current;
    const onPaste = (event) => {
      if (!visibleRef.current || !focusedRef.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      const files = transferFiles(event.clipboardData);
      if (files.length === 1 && files[0].type.startsWith("image/")) receive(imagePayload(files[0]));
      else if (files.length) receive({ files });
      else if (event.clipboardData?.types.includes("text/plain")) receive({ text: event.clipboardData.getData("text/plain") });
    };
    const onDragOver = (event) => {
      if (!visibleRef.current || !hasFiles(event.dataTransfer)) return;
      event.preventDefault(); event.dataTransfer.dropEffect = "copy";
    };
    const onDrop = (event) => {
      if (!visibleRef.current || !hasFiles(event.dataTransfer)) return;
      event.preventDefault(); event.stopPropagation();
      receive({ files: transferFiles(event.dataTransfer), kind: "drop" });
      terminal.focus();
    };
    host.addEventListener("paste", onPaste, true);
    host.addEventListener("dragover", onDragOver);
    host.addEventListener("drop", onDrop);
    const copySelection = () => {
      const text = terminal.getSelection();
      if (!text) {
        // Claude owns its TUI selection while mouse reporting is enabled. CSI-u
        // preserves Ctrl+Shift+C, which cannot be represented as plain Ctrl+C.
        if (terminal.modes.mouseTrackingMode !== "none")
          void enqueue(() => send("\x1b[99;6u")).catch(() => {});
        onStatus(terminal.modes.mouseTrackingMode === "none"
          ? "请先拖选要复制的终端文字"
          : `当前程序接管了鼠标。请按住 ${isMac ? "Option" : "Shift"} 拖选文字，再复制。`);
        return;
      }
      void writeClipboard(text)
        .then(() => onStatus("已复制终端选中文本"))
        .catch(() => onStatus("无法写入本机剪贴板"));
    };
    const onCopy = (event) => {
      if (!visibleRef.current || !focusedRef.current) return;
      event.preventDefault(); event.stopImmediatePropagation();
      const text = terminal.getSelection();
      if (text) event.clipboardData?.setData("text/plain", text);
      copySelection();
    };
    host.addEventListener("copy", onCopy, true);
    onControls?.({
      edit(action) {
        if (!box.current?.contains(document.activeElement)) return false;
        if (action === "copy") {
          copySelection();
          return true;
        }
        if (action === "paste") {
          receive(readClipboardContent());
          return true;
        }
        if (action === "selectAll") {
          terminal.selectAll();
          return true;
        }
        return false;
      },
    });
    terminal.attachCustomKeyEventHandler((event) => {
      if (event.key === "Enter" && event.shiftKey && !event.metaKey && !event.ctrlKey && !event.altKey &&
          !event.isComposing && event.keyCode !== 229) {
        event.preventDefault();
        if (event.type === "keydown") {
          // Bracketed insertion adds a line inside shell/CLI editors. Programs
          // without paste mode receive the distinct CSI-u Shift+Enter key.
          void enqueue(() => send(terminal.modes.bracketedPasteMode
            ? "\x1b[200~\n\x1b[201~" : "\x1b[13;2u")).catch(() => {});
        }
        return false;
      }
      if (event.type !== "keydown") return true;
      const claudeImagePaste = ((session.terminalKind || session.kind) === "claude" || session.cli_session_id) &&
        event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey && event.key.toLowerCase() === "v";
      if (claudeImagePaste && nativeAvailable()) {
        event.preventDefault();
        receive(readClipboardContent());
        return false;
      }
      if (!terminalModifier(event)) return true;
      const key = event.key.toLowerCase();
      if (key === "c") {
        event.preventDefault();
        copySelection();
        return false;
      }
      if (key === "v") {
        if (!nativeAvailable()) return true;
        event.preventDefault();
        receive(readClipboardContent());
        return false;
      }
      if (key === "a") {
        event.preventDefault();
        terminal.selectAll();
        return false;
      }
      if (["+", "=", "-"].includes(key)) {
        event.preventDefault();
        terminal.options.fontSize = Math.max(
          10,
          Math.min(26, terminal.options.fontSize + (key === "-" ? -1 : 1)),
        );
        resize();
        return false;
      }
      return !terminalModifier(event);
    });
    const observer = new ResizeObserver(resize);
    observer.observe(box.current);
    resize();
    return () => {
      disposed = true;
      clearTimeout(startupTimer);
      revealStartup.current = null;
      startupRender.dispose();
      clipboardHandler.dispose();
      host.removeEventListener("paste", onPaste, true);
      host.removeEventListener("copy", onCopy, true);
      host.removeEventListener("dragover", onDragOver);
      host.removeEventListener("drop", onDrop);
      onControls?.(null);
      observer.disconnect();
      ws.close();
      terminal.dispose();
      instance.current = null;
    };
  }, [session.id, attempt]);
  useEffect(() => {
    if (visible) {
      fit.current?.fit();
      if (focused && !preparing) instance.current?.focus();
      const ws = socket.current,
        t = instance.current;
      if (ws?.readyState === WebSocket.OPEN && t)
        ws.send(JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }));
    }
  }, [visible, focused, preparing]);
  useEffect(() => {
    if (!instance.current) return;
    instance.current.options.theme = terminalTheme(theme, variant);
    instance.current.options.fontFamily = theme.fonts.code || systemCodeFont;
    instance.current.options.fontSize = appearance.codeFontSize;
    if (visibleRef.current) fit.current?.fit();
  }, [theme, variant, appearance.codeFontSize]);
  return (
    <div className={`terminal-view ${visible ? "" : "concealed"}`}>
      {session.workspace_notice && <p className="connection-banner" role="status">{session.workspace_notice}</p>}
      {gapNote && <p className="connection-banner" role="status">{gapNote}</p>}
      {visible && session.cli_session_id && <SessionAccount id={session.cli_session_id} request={terminalAccountRequest} compact
        onChanged={onAccountChanged} />}
      <div className={`terminal-host ${preparing ? "terminal-preparing" : ""}`} ref={box} aria-hidden={preparing || undefined} />
      {preparing && (
        <div className="terminal-startup" role="status">
          <span>正在准备 Claude 会话…</span>
          <button onClick={() => revealStartup.current?.()}>显示启动过程</button>
        </div>
      )}
      {!connected && (
        <div className="terminal-note">
          {session.closed
            ? "终端会话已结束。可点击项目右侧的终端图标新建终端。"
            : "正在连接终端…"}
          {!session.closed && (
            <button onClick={() => setAttempt((v) => v + 1)}>
              <RefreshCw size={13} />
              重新连接
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export { Modal } from "./modal";
