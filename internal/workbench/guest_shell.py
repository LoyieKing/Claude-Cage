#!/usr/bin/python3
"""Cage guest shell service.

The PTY master and the shell live here. An SSH attachment is only a client of
the private Unix socket; closing it detaches and does not kill the shell.
"""

import base64
import fcntl
import hashlib
import hmac
import json
import os
import pty
import select
import signal
import socket
import struct
import subprocess
import sys
import termios
import threading
import time

HELLO = 1
RESULT = 2
CREATE = 3
LIST = 4
ATTACH = 5
INPUT = 6
RESIZE = 7
STOP = 8
OUTPUT = 9
GAP = 10
EXIT = 11
LOST = 12
ERROR = 13
ATTACHED = 14

MAX_FRAME = 1 << 20
DEFAULT_RING = 512 * 1024
SOCKET_PATH = os.environ.get("CAGE_GUEST_SHELL_SOCK", "/run/cage/guest-shell.sock")
TOKEN_PATH = os.environ.get("CAGE_GUEST_SHELL_TOKEN_FILE", "/var/lib/cage/guest-shell.token")


def read_exact(sock, size):
    data = bytearray()
    while len(data) < size:
        chunk = sock.recv(size - len(data))
        if not chunk:
            raise EOFError
        data += chunk
    return bytes(data)


def read_frame(sock):
    raw = read_exact(sock, 4)
    size = struct.unpack(">I", raw)[0]
    if size < 1 or size > MAX_FRAME:
        raise ValueError("frame")
    body = read_exact(sock, size)
    return body[0], body[1:]


def write_frame(sock, lock, kind, payload):
    body = bytes([kind]) + payload
    if len(body) > MAX_FRAME:
        raise ValueError("frame")
    packet = struct.pack(">I", len(body)) + body
    with lock:
        sock.sendall(packet)


def encode(value):
    return json.dumps(value, separators=(",", ":")).encode()


def decode(payload):
    if len(payload) > MAX_FRAME:
        raise ValueError("json")
    value = json.loads(payload.decode())
    if not isinstance(value, dict):
        raise ValueError("json")
    return value


def peer_uid(sock):
    cred = sock.getsockopt(socket.SOL_SOCKET, 17, struct.calcsize("3i"))
    _pid, uid, _gid = struct.unpack("3i", cred)
    return uid


def start_time(pid):
    text = open("/proc/%d/stat" % pid, "r", encoding="utf-8", errors="replace").read()
    rest = text.rsplit(")", 1)[1].split()
    return rest[19]


class Shell:
    def __init__(self, epoch, shell_id, create_id, login, cols, rows, cap):
        self.epoch = epoch
        self.id = shell_id
        self.create_id = create_id
        self.login = bool(login)
        self.cols = int(cols)
        self.rows = int(rows)
        self.resize_seq = 1
        self.resize_writer = "create"
        self.cap = 0 if self.login else cap
        self.ring = bytearray()
        self.start = 0
        self.cond = threading.Condition()
        self.inputs = {}
        self.input_order = []
        self.input_lock = threading.Lock()
        self.master_lock = threading.Lock()
        self.stop_lock = threading.Lock()
        self.stopped = False
        self.exited = False
        self.exit_code = None
        self.master = None
        self.proc = None
        self.generation = ""
        self.state = "starting"
        self.subs = []
        self.sub_lock = threading.Lock()
        self.on_exit = None

    def spawn(self, script, cwd):
        master, slave = pty.openpty()
        try:
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.cols, 0, 0))
            proc = subprocess.Popen(
                ["/bin/bash", "--noprofile", "--norc", "-c", script],
                cwd=cwd,
                stdin=slave,
                stdout=slave,
                stderr=slave,
                start_new_session=True,
                close_fds=True,
            )
        except Exception:
            os.close(master)
            os.close(slave)
            raise
        os.close(slave)
        flags = fcntl.fcntl(master, fcntl.F_GETFL)
        fcntl.fcntl(master, fcntl.F_SETFL, flags | os.O_NONBLOCK)
        self.master = master
        self.proc = proc
        try:
            self.generation = start_time(proc.pid)
        except OSError:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except OSError:
                pass
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
            os.close(master)
            self.master = None
            self.proc = None
            raise
        self.state = "running"
        reader = threading.Thread(target=self._read, name="cage-shell-%s" % self.id[:8], daemon=True)
        try:
            reader.start()
        except RuntimeError:
            self.close_master()
            if self.proc is not None and self.proc.poll() is None:
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except OSError:
                    pass
                try:
                    self.proc.wait(timeout=1)
                except subprocess.TimeoutExpired:
                    pass
            self.state = "exited"
            self.exited = True
            raise

    def info(self):
        alive = self.proc is not None and self.proc.poll() is None and not self.exited
        generation = self.generation
        if alive:
            try:
                current = start_time(self.proc.pid)
            except OSError:
                current = ""
            if current != self.generation:
                alive = False
                self.state = "lost"
        state = self.state if not alive or self.state in ("stopped", "lost", "exited") else "running"
        if not alive and state == "running":
            state = "exited"
        return {
            "shell_id": self.id,
            "create_id": self.create_id,
            "pid": 0 if self.proc is None else self.proc.pid,
            "generation": generation,
            "epoch": self.epoch,
            "cols": self.cols,
            "rows": self.rows,
            "resize_seq": self.resize_seq,
            "resize_writer": self.resize_writer,
            "state": state,
            "login": self.login,
            "offset": self.start + len(self.ring),
        }

    def _read(self):
        while True:
            with self.master_lock:
                master = self.master
            if master is None:
                break
            try:
                readable, _, _ = select.select([master], [], [], 0.2)
            except (OSError, ValueError):
                break
            if not readable:
                if self.proc.poll() is not None:
                    break
                continue
            with self.master_lock:
                if self.master != master:
                    break
                try:
                    data = os.read(master, 65536)
                except BlockingIOError:
                    continue
                except OSError:
                    break
            if not data:
                break
            self._accept(data)
        code = 0
        if self.proc is not None:
            try:
                code = self.proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                code = -1
        with self.cond:
            self.exited = True
            self.exit_code = code
            if self.state == "running":
                self.state = "exited"
            self.cond.notify_all()
        self.close_master()
        if self.on_exit is not None:
            self.on_exit(self)

    def close_master(self):
        with self.master_lock:
            master = self.master
            self.master = None
        if master is not None:
            try:
                os.close(master)
            except OSError:
                pass

    def _store_input(self, input_id, status):
        if input_id in self.inputs:
            self.inputs[input_id] = status
            return
        if len(self.input_order) >= 1024:
            self.inputs.pop(self.input_order.pop(0), None)
        self.input_order.append(input_id)
        self.inputs[input_id] = status

    def _accept(self, data):
        if self.login:
            with self.sub_lock:
                subs = list(self.subs)
            for sub in subs:
                sub.push(data)
            return
        with self.cond:
            self.ring += data
            overflow = len(self.ring) - self.cap
            if self.cap and overflow > 0:
                del self.ring[:overflow]
                self.start += overflow
            self.cond.notify_all()

    def write_input(self, input_id, data):
        with self.input_lock:
            previous = self.inputs.get(input_id)
            if previous:
                return previous
            self._store_input(input_id, "accepted")
            view = memoryview(data)
            status = "written"
            with self.master_lock:
                master = self.master
                if master is None:
                    status = "unknown"
                else:
                    try:
                        while len(view):
                            written = os.write(master, view)
                            if written <= 0:
                                status = "unknown"
                                break
                            view = view[written:]
                    except OSError:
                        status = "unknown"
            self._store_input(input_id, status)
            return status

    def resize(self, cols, rows, seq, writer):
        with self.master_lock:
            if seq <= self.resize_seq:
                return self.cols, self.rows, self.resize_seq, self.resize_writer
            if self.master is not None:
                fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
            self.cols, self.rows = cols, rows
            self.resize_seq = seq
            self.resize_writer = writer or ""
            return self.cols, self.rows, self.resize_seq, self.resize_writer

    def stop(self):
        with self.stop_lock:
            if self.stopped:
                return True
            self.stopped = True
            self.state = "stopped"
            proc = self.proc
            if proc is not None and proc.poll() is None:
                try:
                    if start_time(proc.pid) != self.generation:
                        self.state = "lost"
                        with self.cond:
                            self.exited = True
                            self.cond.notify_all()
                        return True
                except OSError:
                    pass
                try:
                    os.killpg(proc.pid, signal.SIGHUP)
                except (ProcessLookupError, PermissionError):
                    pass
                try:
                    proc.wait(timeout=0.4)
                except subprocess.TimeoutExpired:
                    try:
                        os.killpg(proc.pid, signal.SIGKILL)
                    except (ProcessLookupError, PermissionError):
                        pass
                    try:
                        proc.wait(timeout=1)
                    except subprocess.TimeoutExpired:
                        pass
            self.close_master()
            with self.cond:
                self.exited = True
                self.cond.notify_all()
            if self.on_exit is not None:
                self.on_exit(self)
            return True


class LiveSub:
    def __init__(self):
        self.cond = threading.Condition()
        self.chunks = []
        self.dropped = False
        self.closed = False

    def push(self, data):
        with self.cond:
            if self.closed:
                return
            if len(self.chunks) >= 32:
                self.dropped = True
                self.cond.notify()
                return
            self.chunks.append(data)
            self.cond.notify()


class Service:
    def __init__(self, token):
        self.token = token.encode()
        self.epoch = hashlib.sha256(os.urandom(32)).hexdigest()[:32]
        self.lock = threading.Lock()
        self.shells = {}
        self.by_create = {}
        self.ids = set()
        ring = os.environ.get("CAGE_GUEST_SHELL_RING", "")
        self.ring = int(ring) if ring.isdigit() and int(ring) >= 1024 else DEFAULT_RING
        self.tombstones = {}
        self.tomb_order = []
        self.retired = []
        self.retired_set = set()
        self.ended_order = []

    def _retire(self, create_id):
        if create_id in self.retired_set:
            return True
        if len(self.retired_set) >= 1024:
            return False
        self.retired.append(create_id)
        self.retired_set.add(create_id)
        return True

    def _remember(self, info):
        create_id = info.get("create_id", "")
        if not create_id or create_id in self.retired_set:
            return create_id not in self.retired_set
        kept = {key: info.get(key) for key in ("create_id", "shell_id", "pid", "generation", "epoch", "state", "cols", "rows", "resize_seq", "login")}
        if create_id in self.tombstones:
            self.tombstones[create_id] = kept
            return True
        while len(self.tombstones) >= 256:
            if not self.tomb_order:
                return False
            old = self.tomb_order[0]
            if not self._retire(old):
                return False
            self.tomb_order.pop(0)
            self.tombstones.pop(old, None)
        self.tombstones[create_id] = kept
        self.tomb_order.append(create_id)
        return True

    def retain_finished(self, shell):
        # A command that exits before anyone attaches must keep its ring.
        # Explicit Stop retires the shell; this path does not.
        with self.lock:
            info = shell.info()
            if info.get("state") not in ("stopped", "exited", "lost"):
                info["state"] = "exited"
            self._remember(info)
            if shell.id not in self.ended_order:
                self.ended_order.append(shell.id)
            self._evict_ended()

    def _ended_bytes(self):
        total = 0
        for ident in self.ended_order:
            shell = self.shells.get(ident)
            if shell is not None:
                total += len(shell.ring)
        return total

    def _evict_ended(self):
        while self.ended_order and (len(self.ended_order) > 32 or self._ended_bytes() > 2 * 1024 * 1024):
            ident = self.ended_order.pop(0)
            shell = self.shells.get(ident)
            if shell is not None:
                self._drop_shell(shell)

    def _drop_shell(self, shell):
        current = self.shells.get(shell.id)
        if current is shell:
            self.shells.pop(shell.id, None)
        if self.by_create.get(shell.create_id) == shell.id:
            self.by_create.pop(shell.create_id, None)
        info = shell.info()
        if info.get("state") not in ("stopped", "exited", "lost"):
            info["state"] = "exited"
        self._remember(info)
        shell.ring = bytearray()

    def release_shell(self, shell):
        with self.lock:
            self.ended_order = [ident for ident in self.ended_order if ident != shell.id]
            self._drop_shell(shell)

    def _shell_id(self):
        while True:
            ident = os.urandom(16).hex()
            if ident not in self.ids:
                return ident

    def hello(self, payload):
        got = str(payload.get("token", "")).encode()
        if not hmac.compare_digest(got, self.token):
            raise PermissionError("auth")
        return {"epoch": self.epoch}

    def create(self, payload):
        create_id = str(payload.get("create_id", ""))
        if len(create_id) < 16 or len(create_id) > 128 or any(c not in "0123456789abcdef" for c in create_id):
            raise ValueError("create_id")
        with self.lock:
            if create_id in self.retired_set:
                return {"create_id": create_id, "shell_id": create_id[:32], "pid": 0, "generation": "retired", "epoch": self.epoch, "state": "exited", "cols": 0, "rows": 0, "resize_seq": 0, "login": False, "offset": 0}
            existing = self.by_create.get(create_id)
            if existing:
                shell = self.shells.get(existing)
                if shell is not None:
                    return shell.info()
            if create_id in self.tombstones:
                return self.tombstones[create_id]
            live = 0
            for shell in self.shells.values():
                if shell.state in ("running", "starting"):
                    live += 1
            if live >= 32:
                raise ValueError("limit")
            script = payload.get("script", "")
            if not isinstance(script, str) or not script or len(script) > 65536 or "\x00" in script:
                raise ValueError("script")
            login = bool(payload.get("login"))
            cwd = payload.get("cwd") or ""
            if not isinstance(cwd, str):
                raise ValueError("cwd")
            if login and cwd == "":
                cwd = os.path.expanduser("~")
            if not os.path.isabs(cwd) or not os.path.isdir(cwd):
                raise ValueError("cwd")
            cols = int(payload.get("cols") or 100)
            rows = int(payload.get("rows") or 30)
            if not 2 <= cols < 600 or not 2 <= rows < 400:
                raise ValueError("size")
            ident = self._shell_id()
            if not self._remember({"create_id": create_id, "shell_id": ident, "pid": 0, "generation": "", "epoch": self.epoch, "state": "started", "cols": cols, "rows": rows, "resize_seq": 1, "login": login}):
                raise ValueError("retain")
            self.ids.add(ident)
            shell = Shell(self.epoch, ident, create_id, login, cols, rows, self.ring)
            shell.on_exit = self.retain_finished
            self.shells[ident] = shell
            self.by_create[create_id] = ident
        try:
            shell.spawn(script, cwd)
        except Exception:
            with self.lock:
                self.shells.pop(ident, None)
                if self.by_create.get(create_id) == ident:
                    self.by_create.pop(create_id, None)
                self._remember({"create_id": create_id, "shell_id": ident, "pid": 0, "generation": "", "epoch": self.epoch, "state": "exited", "cols": cols, "rows": rows, "resize_seq": 1, "login": login})
            raise
        with self.lock:
            info = shell.info()
            self._remember(info)
        return info

    def list_shells(self):
        with self.lock:
            shells = [shell.info() for shell in self.shells.values()]
            live = {shell.create_id for shell in self.shells.values()}
            for create_id, info in self.tombstones.items():
                if create_id not in live:
                    shells.append(info)
        return {"epoch": self.epoch, "shells": shells}

    def lookup(self, payload, expect_alive=False):
        if str(payload.get("epoch", "")) != self.epoch:
            return None, "epoch"
        with self.lock:
            shell = self.shells.get(str(payload.get("shell_id", "")))
        if shell is None:
            return None, "shell"
        info = shell.info()
        if int(payload.get("pid") or 0) != info["pid"] or str(payload.get("generation", "")) != info["generation"]:
            return None, "generation"
        if expect_alive and info["state"] not in ("running", "starting"):
            return None, info["state"]
        return shell, ""

    def stop(self, payload):
        shell, reason = self.lookup(payload)
        if shell is None:
            return {"stopped": True, "state": "stopped", "reason": reason or "gone"}
        shell.stop()
        self.release_shell(shell)
        return {"stopped": True, "state": "stopped"}

    def handle(self, sock):
        lock = threading.Lock()
        authed = False
        try:
            while True:
                kind, payload = read_frame(sock)
                if not authed:
                    if kind != HELLO:
                        write_frame(sock, lock, ERROR, encode({"error": "auth"}))
                        return
                    try:
                        result = self.hello(decode(payload))
                    except PermissionError:
                        write_frame(sock, lock, ERROR, encode({"error": "auth"}))
                        return
                    authed = True
                    try:
                        sock.settimeout(None)
                    except OSError:
                        return
                    write_frame(sock, lock, RESULT, encode(result))
                    continue
                self._dispatch(sock, lock, kind, payload)
        except (EOFError, ConnectionError, OSError, ValueError, json.JSONDecodeError):
            return
        finally:
            try:
                sock.close()
            except OSError:
                pass

    def _dispatch(self, sock, lock, kind, payload):
        if kind == CREATE:
            write_frame(sock, lock, RESULT, encode(self.create(decode(payload))))
            return
        if kind == LIST:
            write_frame(sock, lock, RESULT, encode(self.list_shells()))
            return
        if kind == STOP:
            write_frame(sock, lock, RESULT, encode(self.stop(decode(payload))))
            return
        if kind == INPUT:
            body = decode(payload)
            shell, reason = self.lookup(body, expect_alive=True)
            if shell is None:
                write_frame(sock, lock, LOST, encode({"reason": reason or "shell"}))
                return
            raw = base64.b64decode(body.get("data", ""), validate=True)
            if len(raw) > 256 * 1024:
                write_frame(sock, lock, RESULT, encode({"input_id": str(body.get("input_id", "")), "status": "rejected"}))
                return
            status = shell.write_input(str(body.get("input_id", "")), raw)
            write_frame(sock, lock, RESULT, encode({"input_id": body.get("input_id", ""), "status": status}))
            return
        if kind == RESIZE:
            body = decode(payload)
            shell, reason = self.lookup(body)
            if shell is None:
                write_frame(sock, lock, LOST, encode({"reason": reason or "shell"}))
                return
            cols, rows, seq, writer = shell.resize(int(body["cols"]), int(body["rows"]), int(body["seq"]), str(body.get("writer", "")))
            write_frame(sock, lock, RESULT, encode({"cols": cols, "rows": rows, "seq": seq, "writer": writer}))
            return
        if kind == ATTACH:
            self._attach(sock, lock, decode(payload))
            return
        write_frame(sock, lock, ERROR, encode({"error": "op"}))

    def _attach(self, sock, lock, body):
        shell, reason = self.lookup(body)
        if shell is None:
            write_frame(sock, lock, LOST, encode({"reason": reason or "shell"}))
            return
        offset = int(body.get("offset") or 0)
        if offset < 0:
            offset = 0
        if shell.login:
            sub = LiveSub()
            with shell.sub_lock:
                shell.subs.append(sub)
            try:
                self._attach_login(sock, lock, shell, sub)
            finally:
                sub.closed = True
                with shell.sub_lock:
                    if sub in shell.subs:
                        shell.subs.remove(sub)
            return
        info = shell.info()
        gap_from = None
        with shell.cond:
            if offset < shell.start:
                gap_from = offset
                offset = shell.start
            end = shell.start + len(shell.ring)
        write_frame(sock, lock, ATTACHED, encode({
            "offset": offset, "ring_start": shell.start, "end": end,
            "cols": shell.cols, "rows": shell.rows, "pid": info["pid"], "generation": info["generation"],
            "epoch": shell.epoch, "resize_seq": shell.resize_seq, "state": info["state"],
        }))
        if gap_from is not None:
            write_frame(sock, lock, GAP, encode({"from": gap_from, "to": offset}))
        while True:
            with shell.cond:
                while True:
                    end = shell.start + len(shell.ring)
                    if offset < shell.start:
                        gap = (offset, shell.start)
                        offset = shell.start
                        chunk = b""
                        break
                    if offset < end:
                        gap = None
                        chunk = bytes(shell.ring[offset - shell.start:offset - shell.start + 65536])
                        break
                    if shell.exited:
                        gap = None
                        chunk = b""
                        break
                    shell.cond.wait(timeout=0.5)
                    continue
                exited = shell.exited and offset >= shell.start + len(shell.ring)
                code = shell.exit_code
            if gap is not None:
                write_frame(sock, lock, GAP, encode({"from": gap[0], "to": gap[1]}))
                continue
            if chunk:
                packet = struct.pack(">Q", offset) + chunk
                write_frame(sock, lock, OUTPUT, packet)
                offset += len(chunk)
                continue
            if exited:
                write_frame(sock, lock, EXIT, encode({"code": 0 if code is None else code, "offset": offset}))
                return

    def _attach_login(self, sock, lock, shell, sub):
        info = shell.info()
        write_frame(sock, lock, ATTACHED, encode({
            "offset": 0, "ring_start": 0, "end": 0, "cols": shell.cols, "rows": shell.rows,
            "pid": info["pid"], "generation": info["generation"], "epoch": shell.epoch,
            "resize_seq": shell.resize_seq, "state": info["state"], "login": True,
        }))
        while True:
            with sub.cond:
                while not sub.chunks and not sub.dropped and not shell.exited:
                    sub.cond.wait(timeout=0.5)
                dropped = sub.dropped
                sub.dropped = False
                chunk = b""
                if sub.chunks:
                    chunk = sub.chunks.pop(0)
                exited = shell.exited and not sub.chunks
                code = shell.exit_code
            if dropped:
                write_frame(sock, lock, GAP, encode({"from": 0, "to": 0, "omitted": True}))
            if chunk:
                write_frame(sock, lock, OUTPUT, struct.pack(">Q", 0) + chunk)
                continue
            if exited:
                write_frame(sock, lock, EXIT, encode({"code": 0 if code is None else code, "offset": 0}))
                return


def serve():
    token = open(TOKEN_PATH, "r", encoding="utf-8").read().strip()
    if not token:
        raise SystemExit(1)
    path = SOCKET_PATH
    directory = os.path.dirname(path)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    if os.path.exists(path):
        os.unlink(path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    os.chmod(path, 0o600)
    server.listen(64)
    service = Service(token)
    uid = os.getuid()
    active = 0
    active_lock = threading.Lock()
    while True:
        conn, _ = server.accept()
        try:
            if peer_uid(conn) != uid:
                conn.close()
                continue
        except OSError:
            conn.close()
            continue
        with active_lock:
            if active >= 64:
                conn.close()
                continue
            active += 1
        conn.settimeout(5)

        def run(connection):
            nonlocal active
            try:
                service.handle(connection)
            finally:
                with active_lock:
                    active -= 1

        try:
            threading.Thread(target=run, args=(conn,), daemon=True).start()
        except RuntimeError:
            conn.close()
            with active_lock:
                active -= 1


def connect():
    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    client.connect(SOCKET_PATH)

    def pump(src, dst, sock=False):
        try:
            while True:
                data = os.read(src, 65536) if not sock else src.recv(65536)
                if not data:
                    break
                if sock:
                    view = memoryview(data)
                    while len(view):
                        written = os.write(dst, view)
                        if written <= 0:
                            return
                        view = view[written:]
                else:
                    dst.sendall(data)
        except OSError:
            pass
        finally:
            try:
                if sock:
                    os.close(dst)
                else:
                    dst.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    left = threading.Thread(target=pump, args=(0, client, False), daemon=True)
    right = threading.Thread(target=pump, args=(client, 1, True), daemon=True)
    left.start()
    right.start()
    left.join()
    right.join()


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("serve", "connect"):
        raise SystemExit(2)
    if sys.argv[1] == "serve":
        serve()
    else:
        connect()


if __name__ == "__main__":
    main()
