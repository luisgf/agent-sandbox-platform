# A transparent forwarder for the browser spike (#231), on the host end of the rig veth: the rig netns DNATs the fake
# address 93.184.216.34 ports 80/443 to 10.82.0.1:8080/8443; here 8080 sends the request on to the lab proxy in
# absolute form (by its Host header) and 8443 opens a CONNECT to the TLS SNI. Only the lab proxy reaches the Internet.
import socket, sys, threading
UP = ("10.82.0.1", 3129)
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError: pass
    finally:
        try: b.shutdown(socket.SHUT_WR)
        except OSError: pass
def splice(c, u):
    t = threading.Thread(target=pipe, args=(u, c), daemon=True); t.start(); pipe(c, u); t.join(timeout=5)
    for s in (u, c):
        try: s.close()
        except OSError: pass
def http(c):
    try:
        data = b""
        while b"\r\n\r\n" not in data:
            d = c.recv(65536)
            if not d: return c.close()
            data += d
        head, rest = data.split(b"\r\n\r\n", 1)
        lines = head.split(b"\r\n")
        method, path, ver = lines[0].split(b" ", 2)
        host = next((l.split(b":", 1)[1].strip() for l in lines[1:] if l.lower().startswith(b"host:")), b"")
        if not path.startswith(b"http"):
            path = b"http://" + host + path
        u = socket.create_connection(UP, timeout=30)
        u.sendall(b" ".join([method, path, ver]) + b"\r\n" + b"\r\n".join(lines[1:]) + b"\r\n\r\n" + rest)
        u.settimeout(None); splice(c, u)
    except Exception:
        c.close()
def sni(hello):
    try:
        i = 5 + 4 + 2 + 32          # record header, handshake header, version, random
        i += 1 + hello[i]           # session id
        i += 2 + int.from_bytes(hello[i:i+2], "big")   # cipher suites
        i += 1 + hello[i]           # compression
        end = i + 2 + int.from_bytes(hello[i:i+2], "big"); i += 2
        while i + 4 <= end:
            t = int.from_bytes(hello[i:i+2], "big"); l = int.from_bytes(hello[i+2:i+4], "big"); i += 4
            if t == 0:
                n = int.from_bytes(hello[i+3:i+5], "big"); return hello[i+5:i+5+n].decode()
            i += l
    except Exception:
        return None
def tls(c):
    try:
        hello = c.recv(65536)
        name = sni(hello)
        if not name: return c.close()
        u = socket.create_connection(UP, timeout=30)
        u.sendall(f"CONNECT {name}:443 HTTP/1.1\r\nHost: {name}:443\r\n\r\n".encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            d = u.recv(4096)
            if not d: break
            resp += d
        if b" 200 " not in resp.split(b"\r\n", 1)[0]:
            u.close(); return c.close()
        u.sendall(hello); u.settimeout(None); splice(c, u)
    except Exception:
        c.close()
def serve(port, fn):
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((sys.argv[1], port)); s.listen(128)
    while True:
        c, _ = s.accept(); threading.Thread(target=fn, args=(c,), daemon=True).start()
threading.Thread(target=serve, args=(8080, http), daemon=True).start()
serve(8443, tls)
