#!/usr/bin/env python3
"""SIGINT checks on disposable Xvfb displays. Run after make build."""
import os
import pathlib
import signal
import socket
import struct
import subprocess
import tempfile
import threading
import time

os.chdir(pathlib.Path(__file__).resolve().parent.parent)
processes = []
logs = {}
tmp = tempfile.TemporaryDirectory(prefix="remmote-shutdown-")


def start(args, name):
    path = pathlib.Path(tmp.name) / (name + ".log")
    with path.open("w") as output:
        p = subprocess.Popen(args, stdout=output, stderr=subprocess.STDOUT)
    processes.append(p)
    logs[p] = path
    return p


def wait_log(p, text):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if text in logs[p].read_text():
            return
        if p.poll() is not None:
            raise AssertionError(f"exited before {text}: {logs[p].read_text()}")
        time.sleep(.02)
    raise AssertionError(f"missing {text}: {logs[p].read_text()}")


def interrupt(p, label):
    started = time.monotonic()
    p.send_signal(signal.SIGINT)
    rc = p.wait(timeout=4)
    output = logs[p].read_text()
    assert rc == 0, (label, rc, output)
    assert "panic:" not in output and "level=ERROR" not in output, output
    print(f"OK: {label}: clean exit in {time.monotonic()-started:.3f}s", flush=True)


def display():
    p = subprocess.Popen(["Xvfb", "-displayfd", "1", "-noreset", "-screen", "0", "800x600x24"],
                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    processes.append(p)
    number = p.stdout.readline().strip()
    assert number.isdigit(), "Xvfb failed"
    p.stdout.close()
    return ":" + number


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def stalled_client(send_hello):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        port = listener.getsockname()[1]
        ready = threading.Event()
        def peer():
            with listener.accept()[0] as conn:
                conn.recv(4096)
                if send_hello:
                    hello = struct.pack(">HHHBBH", 3, 64, 64, 24, 75, 0)
                    conn.sendall(b"RM\x02\x00" + struct.pack(">I", len(hello)) + hello)
                ready.set()
                while conn.recv(4096):
                    pass
        thread = threading.Thread(target=peer, daemon=True)
        thread.start()
        label = "first-frame wait" if send_hello else "handshake wait"
        p = start(["./bin/remmote-client", "-server", f"127.0.0.1:{port}", "-once"], label)
        assert ready.wait(3), "client never reached mock peer"
        interrupt(p, label)
        thread.join(timeout=1)
        assert not thread.is_alive(), "client socket left open"


try:
    host, viewer = display(), display()
    stalled_client(False)
    stalled_client(True)
    # Cancellation while reconnecting must also stop the clipboard watcher.
    p = start(["./bin/remmote-client", "-display", viewer, "-server", f"127.0.0.1:{free_port()}"], "retry")
    wait_log(p, "clipboard sync enabled")
    interrupt(p, "client reconnect wait")

    port = free_port()
    srv = start(["./bin/remmote-server", "-display", host, "-listen", f"127.0.0.1:{port}"], "server")
    wait_log(srv, "listening")
    args = ["./bin/remmote-client", "-display", viewer, "-server", f"127.0.0.1:{port}", "-fast-scale"]
    cli = start(args, "client1")
    wait_log(cli, "msg=connected")
    time.sleep(.2)
    interrupt(cli, "live client with clipboard")
    assert srv.poll() is None, "client shutdown stopped the server"
    cli = start(args, "client2")
    wait_log(cli, "msg=connected")
    # Include a peer that connected but never sends ClientHello.
    with socket.create_connection(("127.0.0.1", port)):
        interrupt(srv, "server with active and stalled clients")
    interrupt(cli, "client after server shutdown")
    with socket.socket() as probe:
        assert probe.connect_ex(("127.0.0.1", port)) != 0, "listener remained open"

    # Startup waits for a window; cancellation must kill/reap the spawned app.
    pidfile = pathlib.Path(tmp.name) / "child.pid"
    command = f"sh -c 'echo $$ > {pidfile}; exec sleep 30'"
    srv = start(["./bin/remmote-server", "-display", host, "-listen", "127.0.0.1:0", "-exec", command], "startup")
    deadline = time.monotonic() + 3
    while not pidfile.exists() and time.monotonic() < deadline:
        time.sleep(.02)
    assert pidfile.exists(), logs[srv].read_text()
    child = int(pidfile.read_text())
    interrupt(srv, "server during application startup")
    try:
        os.kill(child, 0)
    except ProcessLookupError:
        pass
    else:
        raise AssertionError("spawned application survived Ctrl+C")
    print("PASS: Ctrl+C shutdown checks", flush=True)
finally:
    for p in reversed(processes):
        if p.poll() is None:
            p.kill()
        p.wait()
    tmp.cleanup()
