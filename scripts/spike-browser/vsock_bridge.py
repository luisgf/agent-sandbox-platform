#!/usr/bin/env python3
r"""TCP listeners in front of Cloud Hypervisor "hybrid vsock" ports (stdlib only, Python 3.10+).

  vsock_bridge.py 127.0.0.1:19222 /path/ch.vsock 9222
  vsock_bridge.py --listen 127.0.0.1:16080 --socket /path/ch.vsock --port 6080 \
                  [--listen 127.0.0.1:19222 --socket /path/ch.vsock --port 9222 ...]

The first form is one listener; it can be combined with the second. In the
second, --listen and --port pair up in order and --socket is given once (shared
by every listener) or once per listener. For each accepted TCP connection the
bridge opens the VM's Unix socket, sends "CONNECT <port>\n", expects "OK <n>\n"
and then copies bytes both ways until both sides are done. Bind to loopback
unless you mean to expose the guest.
"""

import argparse
import itertools
import logging
import socket
import sys
import threading
import time

BUFSZ = 64 * 1024
HANDSHAKE_TIMEOUT = 10.0   # Unix connect + CONNECT/OK exchange
MAX_REPLY = 128            # a real reply is "OK <u32>\n"; longer is garbage

log = logging.getLogger("vsock_bridge")
conn_ids = itertools.count(1)


class HandshakeError(Exception):
    pass


def read_line(sock):
    """Read through the first newline one byte at a time, so no payload byte behind it is consumed."""
    line = bytearray()
    while len(line) < MAX_REPLY:
        b = sock.recv(1)
        if not b:
            raise HandshakeError("EOF before the end of the reply line")
        line += b
        if b == b"\n":
            return bytes(line)
    raise HandshakeError("reply line too long")


def vsock_connect(path, port):
    """Open the VMM's Unix socket and ask for a guest vsock port; returns (socket, reply text)."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.settimeout(HANDSHAKE_TIMEOUT)
        s.connect(path)
        s.sendall(b"CONNECT %d\n" % port)
        line = read_line(s)
        if not line.startswith(b"OK "):
            raise HandshakeError(f"unexpected reply {line.rstrip()!r}")
        s.settimeout(None)
        return s, line.rstrip().decode("ascii", "replace")
    except BaseException:
        s.close()
        raise


def pump(src, dst, counts, i):
    """Copy src -> dst until EOF, then pass the EOF on as a half-close so the other direction keeps working."""
    try:
        while True:
            data = src.recv(BUFSZ)
            if not data:
                break
            dst.sendall(data)
            counts[i] += len(data)
    except OSError:
        # Reset or broken pipe: shut both sockets so the sibling pump wakes up and ends too.
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        return
    try:
        dst.shutdown(socket.SHUT_WR)
    except OSError:
        pass   # peer already gone; nothing left to tell it


def handle(client, peer, unix_path, vport, label, cid):
    """Serve one accepted TCP connection: handshake with the VMM, splice, log."""
    who = f"#{cid} {label} peer={peer[0]}:{peer[1]}"
    t0 = time.monotonic()
    try:
        try:
            # Small interactive packets (VNC input, CDP) must not wait on Nagle.
            client.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        except OSError:
            pass
        try:
            up, reply = vsock_connect(unix_path, vport)
        except (OSError, HandshakeError) as e:
            log.info("%s handshake FAILED: %s", who, e)
            return
        try:
            log.info("%s open, handshake OK (%s)", who, reply)
            counts = [0, 0]   # client -> guest, guest -> client
            back = threading.Thread(target=pump, args=(up, client, counts, 1), daemon=True)
            back.start()
            pump(client, up, counts, 0)
            back.join()
            log.info("%s closed, to_guest=%d to_client=%d bytes, %.2fs", who, counts[0], counts[1],
                     time.monotonic() - t0)
        finally:
            up.close()
    finally:
        client.close()


def accept_loop(srv, unix_path, vport, label):
    while True:
        try:
            client, peer = srv.accept()
        except OSError as e:
            if srv.fileno() == -1:
                return
            log.error("accept on %s failed: %s", label, e)
            time.sleep(0.1)   # don't spin on EMFILE and friends
            continue
        threading.Thread(target=handle, args=(client, peer, unix_path, vport, label, next(conn_ids)),
                         daemon=True).start()


def listen(host, port, unix_path, vport):
    label = f"{host}:{port}->vsock:{vport}"
    try:
        family, kind, proto, _, addr = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM,
                                                          flags=socket.AI_PASSIVE)[0]
        srv = socket.socket(family, kind, proto)
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind(addr)
        srv.listen(128)
    except OSError as e:
        sys.exit(f"vsock_bridge.py: cannot listen on {host}:{port}: {e}")
    log.info("listening on %s via %s", label, unix_path)
    threading.Thread(target=accept_loop, args=(srv, unix_path, vport, label), daemon=True).start()


def tcp_addr(text):
    host, sep, port = text.rpartition(":")
    if not sep or not host or not port.isdigit() or int(port) > 65535:
        raise argparse.ArgumentTypeError(f"bad address {text!r} (want HOST:PORT)")
    return host.strip("[]"), int(port)


def vsock_port(text):
    if not text.isdigit() or not 0 < int(text) < 1 << 32:
        raise argparse.ArgumentTypeError(f"bad vsock port {text!r}")
    return int(text)


def main():
    ap = argparse.ArgumentParser(
        prog="vsock_bridge.py", description=__doc__.split("\n\n")[0],
        usage="%(prog)s LISTEN_ADDR:PORT UNIX_SOCKET VSOCK_PORT\n"
              "       %(prog)s --listen HOST:PORT --socket PATH --port VSOCK_PORT [--listen ...]")
    ap.add_argument("simple", nargs="*", help=argparse.SUPPRESS)   # the one-listener form
    ap.add_argument("--listen", action="append", default=[], type=tcp_addr, metavar="HOST:PORT",
                    help="TCP address to listen on (repeatable)")
    ap.add_argument("--socket", action="append", default=[], metavar="PATH",
                    help="the VM's hybrid-vsock Unix socket (once, or once per --listen)")
    ap.add_argument("--port", action="append", default=[], type=vsock_port, metavar="VSOCK_PORT",
                    help="guest vsock port to CONNECT to (once per --listen)")
    args = ap.parse_args()
    n = len(args.listen)
    if len(args.port) != n:
        ap.error("need exactly one --port per --listen")
    if n and len(args.socket) not in (1, n):
        ap.error("give --socket once, or once per --listen")
    if not n and args.socket:
        ap.error("--socket without --listen")
    sockets = args.socket * n if len(args.socket) == 1 else args.socket
    specs = [(host, port, path, vport) for (host, port), path, vport in zip(args.listen, sockets, args.port)]
    if args.simple:
        if len(args.simple) != 3:
            ap.error("the short form is LISTEN_ADDR:PORT UNIX_SOCKET VSOCK_PORT")
        try:
            specs.append((*tcp_addr(args.simple[0]), args.simple[1], vsock_port(args.simple[2])))
        except argparse.ArgumentTypeError as e:
            ap.error(str(e))
    if not specs:
        ap.error("nothing to listen on")
    logging.basicConfig(stream=sys.stderr, level=logging.INFO, format="%(asctime)s %(message)s")
    for spec in specs:
        listen(*spec)
    try:
        threading.Event().wait()   # all the work happens in daemon threads
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
