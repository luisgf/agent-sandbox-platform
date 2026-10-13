#!/usr/bin/env python3
"""Minimal Chrome DevTools Protocol client (stdlib only, Python 3.10+).

  cdp.py [--endpoint http://127.0.0.1:9222] version
  cdp.py [--endpoint ...] wait [--timeout 60]
  cdp.py [--endpoint ...] goto URL [--timeout 60] [--screenshot out.png]

Discovery is plain HTTP (/json/*); the protocol runs over a hand-rolled
RFC 6455 WebSocket client (ws:// only: no TLS, no extensions).

goto prints one JSON line {url, load_ms, title, images, error}. A page that fails
to load is reported there (error set, or load_ms null) with exit status 0; a
non-zero status with a message on stderr means cdp.py itself failed.
"""

import argparse
import base64
import hashlib
import http.client
import json
import os
import socket
import struct
import sys
import time
import urllib.parse

DEFAULT_ENDPOINT = "http://127.0.0.1:9222"
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"  # fixed by RFC 6455
HTTP_TIMEOUT = 10.0      # one /json/* request
SEND_TIMEOUT = 10.0      # outgoing frames are tiny; this only guards a wedged peer
MAX_MESSAGE = 256 << 20  # sanity cap; a full-page PNG screenshot is a few MB


class Fail(Exception):
    """Fatal error; the message goes to stderr as is."""


class Timeout(Fail):
    """A read passed its deadline. Resumable: a timeout consumes no bytes."""


class Closed(Fail):
    """The WebSocket is closed or broken."""


class Endpoint:
    """The browser's DevTools HTTP address."""

    def __init__(self, url):
        if "://" not in url:
            url = "http://" + url
        u = urllib.parse.urlsplit(url)
        try:
            port = u.port
        except ValueError:
            raise Fail(f"bad endpoint {url!r}") from None
        if u.scheme != "http" or not u.hostname:
            raise Fail(f"bad endpoint {url!r} (want http://HOST:PORT)")
        self.host, self.port = u.hostname, port or 80
        self.base = f"http://{u.netloc}"

    def request(self, method, path, timeout=HTTP_TIMEOUT):
        """One request on a fresh connection; returns (status, body). http.client never uses a proxy."""
        conn = http.client.HTTPConnection(self.host, self.port, timeout=timeout)
        try:
            conn.request(method, path)
            resp = conn.getresponse()
            return resp.status, resp.read()
        except (OSError, http.client.HTTPException) as e:
            raise Fail(f"{method} {self.base}{path}: {e}") from None
        finally:
            conn.close()

    def fetch(self, method, path, timeout=HTTP_TIMEOUT):
        status, body = self.request(method, path, timeout)
        if status != 200:
            snippet = body[:200].decode("utf-8", "replace")
            raise Fail(f"{method} {self.base}{path}: HTTP {status}: {snippet}")
        try:
            return json.loads(body)
        except ValueError:
            raise Fail(f"{method} {self.base}{path}: reply is not JSON") from None


def mask(data, key):
    """XOR with the repeating 4-byte key as one big-int op; a per-byte Python loop is too slow for MB payloads."""
    n = len(data)
    k = (key * (n // 4 + 1))[:n]
    return (int.from_bytes(data, "big") ^ int.from_bytes(k, "big")).to_bytes(n, "big")


class WebSocket:
    """Client side of RFC 6455 over a plain TCP socket."""

    def __init__(self, sock):
        self.sock = sock
        self.buf = bytearray()   # received, not yet parsed
        self.deadline = 0.0      # time.monotonic() limit for reads
        self.msg_op = None       # opcode of the fragmented message in progress...
        self.msg_parts = []      # ...its fragments so far...
        self.msg_len = 0         # ...and their total size
        self.sent_close = False
        self.got_close = False

    @classmethod
    def connect(cls, host, port, path, timeout):
        try:
            sock = socket.create_connection((host, port), timeout=timeout)
        except OSError as e:
            raise Fail(f"cannot connect to {host}:{port}: {e}") from None
        ws = cls(sock)
        try:
            ws._handshake(f"[{host}]:{port}" if ":" in host else f"{host}:{port}", path, timeout)
        except BaseException:
            sock.close()
            raise
        return ws

    def _handshake(self, hostport, path, timeout):
        key = base64.b64encode(os.urandom(16)).decode()
        # No Origin header on purpose: Chromium >= 111 rejects origins it was not told to allow.
        req = (f"GET {path} HTTP/1.1\r\nHost: {hostport}\r\nUpgrade: websocket\r\n"
               f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        try:
            self.sock.settimeout(SEND_TIMEOUT)
            self.sock.sendall(req.encode())
        except OSError as e:
            raise Fail(f"websocket handshake: {e}") from None
        self.deadline = time.monotonic() + timeout
        while b"\r\n\r\n" not in self.buf:
            if len(self.buf) > 65536:
                raise Fail("websocket handshake: reply too large")
            self._more()
        head, _, rest = bytes(self.buf).partition(b"\r\n\r\n")
        self.buf = bytearray(rest)   # frames may already follow the reply
        lines = head.decode("latin-1").split("\r\n")
        headers = {k.strip().lower(): v.strip() for k, _, v in (ln.partition(":") for ln in lines[1:])}
        if lines[0].split(" ")[1:2] != ["101"]:
            body = rest[:200].decode("latin-1").strip()
            raise Fail(f"websocket handshake refused: {lines[0]}" + (f" ({body})" if body else ""))
        want = base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
        if headers.get("sec-websocket-accept") != want:
            raise Fail("websocket handshake: bad Sec-WebSocket-Accept")

    def _more(self):
        """Append one chunk from the socket to the buffer, within self.deadline."""
        left = self.deadline - time.monotonic()
        if left <= 0:
            raise Timeout("timed out waiting for the browser")
        self.sock.settimeout(left)
        try:
            chunk = self.sock.recv(1 << 18)
        except TimeoutError:
            raise Timeout("timed out waiting for the browser") from None
        except OSError as e:
            raise Closed(f"websocket read failed: {e}") from None
        if not chunk:
            raise Closed("connection closed by the browser")
        self.buf += chunk

    def _need(self, n):
        while len(self.buf) < n:
            self._more()

    def _read_frame(self):
        """Return (fin, opcode, payload).

        Consumes bytes only once the whole frame is buffered, so a Timeout loses nothing.
        """
        self._need(2)
        b0, b1 = self.buf[0], self.buf[1]
        if b0 & 0x70:
            raise Fail("websocket: reserved bits set (no extension was negotiated)")
        if b1 & 0x80:
            raise Fail("websocket: server frames must not be masked")
        n, hdr = b1 & 0x7F, 2
        if n == 126:
            self._need(4)
            n, hdr = struct.unpack(">H", self.buf[2:4])[0], 4
        elif n == 127:
            self._need(10)
            n, hdr = struct.unpack(">Q", self.buf[2:10])[0], 10
        if n > MAX_MESSAGE:
            raise Fail(f"websocket: {n}-byte frame is too large")
        self._need(hdr + n)
        payload = bytes(self.buf[hdr:hdr + n])
        del self.buf[:hdr + n]
        return bool(b0 & 0x80), b0 & 0x0F, payload

    def recv_message(self, deadline):
        """Next complete data message: str for text, bytes for binary.

        Control frames are handled on the way (ping -> pong, close -> echo then Closed);
        fragments are reassembled, and the partial state survives a Timeout.
        """
        self.deadline = deadline
        while True:
            fin, op, payload = self._read_frame()
            if op >= 8:
                if not fin or len(payload) > 125:
                    raise Fail("websocket: malformed control frame")
                if op == 9:
                    self._send_frame(10, payload)
                elif op == 8:
                    self.got_close = True
                    if not self.sent_close:
                        self.sent_close = True
                        self._send_frame(8, payload[:2])   # echo the status code
                    code = struct.unpack(">H", payload[:2])[0] if len(payload) >= 2 else None
                    raise Closed(f"browser closed the websocket (code {code})")
                elif op != 10:
                    raise Fail(f"websocket: unknown opcode {op}")
                continue
            if op == 0:
                if self.msg_op is None:
                    raise Fail("websocket: continuation frame with nothing to continue")
            elif op in (1, 2):
                if self.msg_op is not None:
                    raise Fail("websocket: new message inside a fragmented one")
                self.msg_op = op
            else:
                raise Fail(f"websocket: unknown opcode {op}")
            self.msg_parts.append(payload)
            self.msg_len += len(payload)
            if self.msg_len > MAX_MESSAGE:
                raise Fail("websocket: message is too large")
            if fin:
                break
        data, op = b"".join(self.msg_parts), self.msg_op
        self.msg_op, self.msg_parts, self.msg_len = None, [], 0
        if op == 2:
            return data
        try:
            return data.decode("utf-8")   # decode after reassembly: a character can span fragments
        except UnicodeDecodeError:
            raise Fail("websocket: invalid UTF-8 in a text message") from None

    def _send_frame(self, op, payload=b""):
        n = len(payload)
        head = bytearray([0x80 | op])   # FIN: outgoing messages are never fragmented
        if n < 126:
            head.append(0x80 | n)       # 0x80 = MASK: client frames must be masked
        elif n < 1 << 16:
            head += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", n)
        key = os.urandom(4)
        try:
            self.sock.settimeout(SEND_TIMEOUT)
            self.sock.sendall(bytes(head) + key + mask(payload, key))
        except OSError as e:
            raise Closed(f"websocket write failed: {e}") from None

    def send_text(self, text):
        self._send_frame(1, text.encode())

    def close(self):
        """Closing handshake (send Close, wait briefly for the echo), then drop the TCP connection."""
        try:
            if not self.sent_close:
                self.sent_close = True
                self._send_frame(8, struct.pack(">H", 1000))
            self.deadline = time.monotonic() + 2.0
            while not self.got_close:
                _, op, _ = self._read_frame()   # data frames are discarded: we are leaving
                self.got_close = op == 8
        except (OSError, Fail):
            pass
        finally:
            self.sock.close()


class Session:
    """CDP over one WebSocket: replies matched to calls by id, events buffered as they arrive."""

    def __init__(self, ws):
        self.ws = ws
        self.last_id = 0
        self.events = []   # (time.monotonic() at receipt, event message)

    def _read(self, deadline):
        raw = self.ws.recv_message(deadline)
        try:
            msg = json.loads(raw)
        except ValueError:
            raise Fail("the browser sent a message that is not JSON") from None
        if not isinstance(msg, dict):
            raise Fail("the browser sent an unexpected message")
        if "method" in msg and "id" not in msg:
            self.events.append((time.monotonic(), msg))
        return msg

    def call(self, method, params=None, timeout=60.0):
        self.last_id += 1
        want = self.last_id
        self.ws.send_text(json.dumps({"id": want, "method": method, "params": params or {}}))
        deadline = time.monotonic() + timeout
        while True:
            try:
                msg = self._read(deadline)
            except Timeout:
                raise Timeout(f"{method}: no reply within {timeout:g}s") from None
            if msg.get("id") == want:
                if "error" in msg:
                    err = msg["error"]
                    raise Fail(f"{method} failed: {err.get('message', err) if isinstance(err, dict) else err}")
                return msg.get("result") or {}

    def wait_event(self, name, deadline):
        """(receipt time, message) of the first `name` event, already buffered or yet to come; None at the deadline."""
        seen = 0
        while True:
            for item in self.events[seen:]:
                if item[1]["method"] == name:
                    return item
            seen = len(self.events)
            try:
                self._read(deadline)
            except Timeout:
                return None

    def evaluate(self, expression, timeout):
        res = self.call("Runtime.evaluate", {"expression": expression, "returnByValue": True}, timeout)
        if "exceptionDetails" in res:
            raise Fail(f"evaluating {expression!r} threw: {res['exceptionDetails'].get('text')}")
        return res.get("result", {}).get("value")


def cmd_version(ep, args):
    print(json.dumps(ep.fetch("GET", "/json/version"), indent=2))
    return 0


def cmd_wait(ep, args):
    t0 = time.monotonic()
    deadline = t0 + args.timeout
    last = ""
    while True:
        try:
            # Short per-try timeout: a half-up bridge can accept the TCP connection and then stall.
            status, _ = ep.request("GET", "/json/version", timeout=min(2.0, max(0.05, deadline - time.monotonic())))
            if status == 200:
                print(f"READY {time.monotonic() - t0:.3f}")
                return 0
            last = f"HTTP {status}"
        except Fail as e:
            last = str(e)
        if time.monotonic() >= deadline:
            break
        time.sleep(min(0.1, max(0.0, deadline - time.monotonic())))
    print("NOT READY")
    print(f"cdp.py: no answer from {ep.base} within {args.timeout:g}s (last try: {last})", file=sys.stderr)
    return 1


def first_page(ep):
    """First page target that can be attached to; opens a blank one if the browser has none."""
    targets = ep.fetch("GET", "/json/list")
    if not isinstance(targets, list):
        raise Fail("/json/list: unexpected reply")
    pages = [t for t in targets if isinstance(t, dict) and t.get("type") == "page"]
    if not pages:
        pages = [ep.fetch("PUT", "/json/new?about:blank")]   # newer Chromium rejects GET here
    for t in pages:
        if isinstance(t, dict) and t.get("webSocketDebuggerUrl"):
            return t
    raise Fail("no page target has a webSocketDebuggerUrl (is another DevTools client attached?)")


def cmd_goto(ep, args):
    page = first_page(ep)
    # Dial the endpoint's own host:port and take only the path from the advertised URL:
    # its authority just echoes the Host header we sent, which is wrong behind a port bridge.
    u = urllib.parse.urlsplit(page["webSocketDebuggerUrl"])
    ws = WebSocket.connect(ep.host, ep.port, u.path + ("?" + u.query if u.query else ""), args.timeout)
    try:
        s = Session(ws)
        s.call("Page.enable", timeout=args.timeout)
        s.events.clear()   # a load event from whatever was loading before is not ours
        t0 = time.monotonic()
        res = s.call("Page.navigate", {"url": args.url}, timeout=args.timeout)
        error = res.get("errorText")
        load_ms = None
        if not error:
            ev = s.wait_event("Page.loadEventFired", t0 + args.timeout)
            if ev:
                load_ms = round((ev[0] - t0) * 1000)
            else:
                error = f"no load event within {args.timeout:g}s"   # ours, but reported like the browser's
        title = s.evaluate("document.title", args.timeout)
        images = s.evaluate("document.querySelectorAll('img').length", args.timeout)
        if args.screenshot:
            data = s.call("Page.captureScreenshot", {"format": "png"}, args.timeout).get("data")
            if not data:
                raise Fail("Page.captureScreenshot returned no data")
            try:
                with open(args.screenshot, "wb") as f:
                    f.write(base64.b64decode(data))
            except (OSError, ValueError) as e:
                raise Fail(f"cannot write {args.screenshot}: {e}") from None
    finally:
        ws.close()
    # The single JSON line is the whole result, also when the page did not load ("error", null "load_ms"):
    # callers tail it with stderr merged in, so nothing else is printed. Non-zero exit = cdp.py itself failed.
    print(json.dumps({"url": args.url, "load_ms": load_ms, "title": title, "images": images, "error": error},
                     separators=(",", ":")))
    return 0


def seconds(text):
    value = float(text)
    if value <= 0:
        raise argparse.ArgumentTypeError("must be greater than 0")
    return value


def main(argv=None):
    ap = argparse.ArgumentParser(prog="cdp.py", description="Minimal Chrome DevTools Protocol client.")
    ap.add_argument("--endpoint", default=DEFAULT_ENDPOINT, metavar="URL",
                    help="DevTools HTTP endpoint (default: %(default)s)")
    # Also accept --endpoint after the subcommand; SUPPRESS keeps the subparser from resetting a value given before it.
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--endpoint", default=argparse.SUPPRESS, metavar="URL", help=argparse.SUPPRESS)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("version", parents=[common], help="print /json/version")
    p = sub.add_parser("wait", parents=[common], help="poll until the browser answers")
    p.add_argument("--timeout", type=seconds, default=60.0, help="give up after this many seconds (default: 60)")
    p = sub.add_parser("goto", parents=[common], help="navigate the first page and report the result as JSON")
    p.add_argument("url")
    p.add_argument("--timeout", type=seconds, default=60.0,
                   help="limit for each CDP reply, and for the load event counted from the navigate (default: 60)")
    p.add_argument("--screenshot", metavar="FILE", help="also save a PNG screenshot")
    args = ap.parse_args(argv)
    try:
        return {"version": cmd_version, "wait": cmd_wait, "goto": cmd_goto}[args.cmd](Endpoint(args.endpoint), args)
    except Fail as e:
        print(f"cdp.py: error: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
