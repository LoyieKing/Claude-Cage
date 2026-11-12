import test from "node:test";
import assert from "node:assert/strict";
import { api, native } from "./api.js";
import { clientKeyboard, clientStorageKey, runtimeURL, setClientContext } from "./client-context.mjs";
import { recoveryDecision, transportGeneration } from "./client-context.mjs";

function environment(t) {
  const saved = { fetch: globalThis.fetch, window: globalThis.window, location: globalThis.location };
  globalThis.location = new URL("http://localhost:17777/");
  globalThis.window = { webkit: { messageHandlers: { cage: { postMessage: async () => true } } } };
  setClientContext({ mode: "remote", connection_id: "first", instance_id: "instance-one" });
  t.after(() => {
    setClientContext({ mode: "local", connection_id: "local" });
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete globalThis[key]; else globalThis[key] = value;
    }
  });
}

test("an old API result cannot continue an action in a new remote environment", async (t) => {
  environment(t);
  let release;
  globalThis.fetch = (path, options) => {
    assert.equal(path, "/api/input/files/create");
    assert.equal(options.headers["X-Cage-Connection"], "first");
    return new Promise((resolve) => { release = resolve; });
  };
  const pending = api("/input/files/create", { target: "browser" });
  setClientContext({ mode: "remote", connection_id: "second", instance_id: "instance-two" });
  release(new Response(JSON.stringify({ id: "old-upload" })));
  await assert.rejects(pending, (error) => error.code === "connection_changed");
});

test("a native picker finishing after a host switch cannot yield an upload handle", async (t) => {
  environment(t);
  let release;
  window.webkit.messageHandlers.cage.postMessage = (request) => {
    assert.equal(request.connection_id, "first");
    return new Promise((resolve) => { release = resolve; });
  };
  const pending = native("chooseUploadFiles", { multiple: true });
  setClientContext({ mode: "remote", connection_id: "second" });
  release({ fileSelection: "old-file-handle", count: 1 });
  await assert.rejects(pending, (error) => error.code === "connection_changed");
});

test("quitting a remote client never suspends the cloud account", async (t) => {
  environment(t);
  globalThis.fetch = () => { throw new Error("quit must not call a runtime API"); };
  let request;
  window.webkit.messageHandlers.cage.postMessage = async (value) => { request = value; return true; };
  await native("quitNow");
  assert.deepEqual(request, { action: "quitNow", connection_id: "first" });
});

const localGateway = (state) => ({
  mode: "local", desktop_gateway: true, connection_id: "local-view",
  connection: { state, error_code: state === "failed" ? "connection_lost" : "" },
});

test("quit still reaches the desktop after the selected CLI controller exits", async (t) => {
  environment(t);
  setClientContext(localGateway("connected"));
  const calls = [];
  let status;
  globalThis.fetch = async (path) => {
    calls.push(path);
    if (path === "/api/client/context") return Response.json(localGateway("failed"));
    return new Response("connection unavailable", { status });
  };
  window.webkit.messageHandlers.cage.postMessage = ({ action }) => calls.push(action);
  for (status of [502, 409]) {
    calls.length = 0;
    await native("quitNow");
    assert.deepEqual(calls, ["/api/account/suspend", "/api/client/context", "quitNow"]);
  }
  calls.length = 0;
  setClientContext(localGateway("failed"));
  await native("quitNow");
  assert.deepEqual(calls, ["quitNow"]);
});

test("a disconnected view from another generation cannot authorize a pending quit", async (t) => {
  environment(t);
  setClientContext(localGateway("connected"));
  globalThis.fetch = async (path) => path === "/api/client/context"
    ? Response.json({ ...localGateway("failed"), connection_id: "replacement" })
    : new Response("view changed", { status: 409 });
  window.webkit.messageHandlers.cage.postMessage = () => assert.fail("stale quit");
  await assert.rejects(native("quitNow"), { status: 409 });
});

test("quit still reaches the desktop when the CLI HTTP listener is gone", async (t) => {
  environment(t);
  setClientContext({ mode: "local", connection_id: "local" });
  globalThis.fetch = async (path) => {
    if (path === "/api/diagnostics/events") return Response.json({});
    assert.equal(path, "/api/account/suspend");
    throw new TypeError("Failed to fetch");
  };
  const actions = [];
  window.webkit.messageHandlers.cage.postMessage = ({ action }) => actions.push(action);
  await native("quitNow");
  assert.deepEqual(actions, ["quitNow"]);
});

test("an offline quit cannot cross a connection change during account suspension", async (t) => {
  environment(t);
  setClientContext(localGateway("connected"));
  let failRequest;
  globalThis.fetch = (path) => {
    if (path === "/api/diagnostics/events") return Promise.resolve(Response.json({}));
    return new Promise((resolve, reject) => { failRequest = reject; });
  };
  window.webkit.messageHandlers.cage.postMessage = () => assert.fail("stale quit");
  const quitting = native("quitNow");
  setClientContext({ ...localGateway("connected"), connection_id: "replacement" });
  failRequest(new TypeError("Failed to fetch"));
  await assert.rejects(quitting, { code: "connection_changed" });
});

test("a reachable local controller must lock or report already locked before quit", async (t) => {
  environment(t);
  setClientContext(localGateway("connected"));
  window.dispatchEvent = () => {};
  const calls = [];
  let status = 500;
  globalThis.fetch = async (path) => {
    calls.push(path);
    if (path === "/api/client/context") return Response.json(localGateway("connected"));
    return Response.json({}, { status });
  };
  window.webkit.messageHandlers.cage.postMessage = ({ action }) => calls.push(action);
  for (status of [500, 409, 502]) {
    await assert.rejects(native("quitNow"), { status });
    assert.ok(!calls.includes("quitNow"));
  }
  for (status of [200, 423]) {
    calls.length = 0;
    await native("quitNow");
    assert.deepEqual(calls, ["/api/account/suspend", "quitNow"]);
  }
});

test("an account device operation retains its host generation and rejects a late result", async (t) => {
  environment(t);
  let release;
  window.webkit.messageHandlers.cage.postMessage = (request) => {
    assert.deepEqual(request, { action: "accountDevice", operation: "unlock", kind: "automatic", connection_id: "first" });
    return new Promise((resolve) => { release = resolve; });
  };
  const pending = native("accountDevice", { operation: "unlock", kind: "automatic" });
  setClientContext({ mode: "remote", connection_id: "second" });
  release({ unlocked: true });
  await assert.rejects(pending, (error) => error.code === "connection_changed");
});

test("stream URLs carry the generation while layout follows the persistent instance", (t) => {
  environment(t);
  assert.equal(runtimeURL("/browser/events?after=1"), "/api/browser/events?after=1&connection=first");
  const key = clientStorageKey("cage-tab-layout");
  setClientContext({ mode: "remote", connection_id: "reconnected", instance_id: "instance-one" });
  assert.equal(clientStorageKey("cage-tab-layout"), key);
  setClientContext({ mode: "remote", connection_id: "other", instance_id: "instance-two" });
  assert.notEqual(clientStorageKey("cage-tab-layout"), key);
});

test("outer reconnect adopts the same instance without replaying or sending a retired SSH id", (t) => {
  environment(t);
  const previous = {
    mode: "remote", desktop_gateway: true, connection_id: "ssh-old", instance_id: "instance-one",
    connection: { state: "connected", connection_id: "ssh-old", instance_id: "instance-one" },
  };
  const next = {
    ...previous, connection_id: "ssh-new",
    connection: { state: "connected", connection_id: "ssh-new", instance_id: "instance-one" },
  };
  assert.deepEqual(recoveryDecision(previous, next), { reload: true, reattachReadonly: false, dropPending: true });
  assert.deepEqual(recoveryDecision(previous, { ...previous, connection: { ...previous.connection, state: "reconnecting" } }), {
    reload: false, reattachReadonly: false, dropPending: false,
  });
  setClientContext(next);
  assert.equal(runtimeURL("/browser/events"), "/api/browser/events?connection=ssh-new");
  assert.equal(transportGeneration(), "ssh-new");
  const simplePrevious = { mode: "remote", connection_id: "ssh-old", instance_id: "instance-one", connection: { connection_id: "ssh-old" } };
  const simpleNext = { mode: "remote", connection_id: "ssh-new", instance_id: "instance-one", connection: { connection_id: "ssh-new" } };
  assert.equal(recoveryDecision(simplePrevious, simpleNext).reload, true);
  assert.equal(recoveryDecision(simplePrevious, { ...simpleNext, instance_id: "instance-two" }).reload, true);
  assert.equal(recoveryDecision(simplePrevious, { mode: "local", connection_id: "local" }).reload, true);
});

test("a late API or native result cannot continue on the same instance after the transport id rotates", async (t) => {
  environment(t);
  let release;
  globalThis.fetch = () => new Promise((resolve) => { release = resolve; });
  const pending = api("/input/files/create", { target: "browser" });
  setClientContext({
    mode: "remote", desktop_gateway: true, connection_id: "ssh-new", instance_id: "instance-one",
    connection: { state: "connected", connection_id: "ssh-new", instance_id: "instance-one" },
  });
  release(new Response(JSON.stringify({ id: "old-upload" })));
  await assert.rejects(pending, (error) => error.code === "connection_changed");
  let nativeRelease;
  window.webkit.messageHandlers.cage.postMessage = () => new Promise((resolve) => { nativeRelease = resolve; });
  setClientContext({
    mode: "remote", desktop_gateway: true, connection_id: "ssh-old", instance_id: "instance-one",
    connection: { state: "connected", connection_id: "ssh-old", instance_id: "instance-one" },
  });
  const picked = native("chooseUploadFiles", { multiple: true });
  setClientContext({
    mode: "remote", desktop_gateway: true, connection_id: "ssh-new", instance_id: "instance-one",
    connection: { state: "connected", connection_id: "ssh-new", instance_id: "instance-one" },
  });
  nativeRelease({ fileSelection: "old-file-handle", count: 1 });
  await assert.rejects(picked, (error) => error.code === "connection_changed");
});

test("remote keyboard defaults follow the client and a saved choice does not alter server settings", (t) => {
  environment(t);
  const previous = globalThis.localStorage;
  const values = new Map();
  globalThis.localStorage = { getItem: (key) => values.get(key) ?? null };
  t.after(() => { if (previous === undefined) delete globalThis.localStorage; else globalThis.localStorage = previous; });
  const mac = { mode: "remote", client_platform: "darwin", instance_id: "linux-server" };
  const windows = { mode: "remote", client_platform: "windows", instance_id: "linux-server" };
  assert.equal(clientKeyboard("native", mac), "mac");
  assert.equal(clientKeyboard("mac", windows), "native");
  values.set(clientStorageKey("cage-keyboard", mac), "native");
  assert.equal(clientKeyboard("mac", mac), "native");
  assert.equal(clientKeyboard("native", { ...mac, instance_id: "other-server" }), "mac");
  assert.equal(clientKeyboard("native", { mode: "local", client_platform: "darwin" }), "native");
});
