#!/usr/bin/env python3
"""How long a microVM takes to start, and what a Cloud Hypervisor snapshot would change.

The measurements behind docs/adr/0017-fast-start.md. It boots a throwaway VM with the kernel and the
guest image a node boots, straight through Cloud Hypervisor's REST API, and times what a session start
pays: the disk, the guest, the pod-daemon answering over vsock. Nothing of ASP is touched: it uses a
directory, a TAP and a vsock socket of its own (the TAP is not called asp-*, which a node-agent would
remove when it starts), and cleans up after itself.

    scripts/bench-vm-start.py [options] [cold] [snapshot] [big]

  cold      the cold start in four disk set-ups (a fresh copy, a flushed copy, a qcow2 overlay, an
            overlay on an image whose workspace helper does not wait), with and without a workspace
  snapshot  pause + snapshot + restore of a 512 MiB guest: time, size, what survives, the clock, a
            virtiofs workspace with open files
  big       the same for guests of 2 and 4 GiB, with the snapshot in and out of the page cache

Needs a Linux host with KVM, cloud-hypervisor, virtiofsd and qemu-img on the PATH (or --ch and
friends), a guest kernel and image (what `asp image pull` installs), sudo for the TAP, loop mounts and
setpriv, and Python 3 with nothing else. It is meant for a host you can spare a few GiB and a minute of
CPU on, not for a node that is serving sandboxes at that moment.
"""
import argparse
import http.client
import json
import os
import shutil
import socket
import statistics
import subprocess
import sys
import time
import traceback

TAP, HOST_IP, GUEST_IP = "benchtap0", "10.250.250.1", "10.250.250.2"
CMDLINE = f"console=ttyS0 root=/dev/vda reboot=k panic=1 ip={GUEST_IP}::{HOST_IP}:255.255.255.252:bench:eth0:off"

args = None  # the parsed options
procs = []  # every process started, so cleanup can kill what a failure left
mem_mib = 512
ambient_ptrace = False  # start Cloud Hypervisor with CAP_SYS_PTRACE (what on-demand restore needs)


def sh(*cmd, check=True):
    return subprocess.run(cmd, check=check, capture_output=True, text=True)


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path_ = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path_)


def ch_api(sock, method, path, body=None, timeout=120):
    c = UnixHTTP(sock, timeout)
    c.request(method, "/api/v1/" + path, body=json.dumps(body) if body is not None else None,
              headers={"Content-Type": "application/json"})
    r = c.getresponse()
    data = r.read().decode(errors="replace")
    c.close()
    return r.status, data


def vsock_http(vsock, method, path, body=None, port=26500, timeout=3):
    """One HTTP request to the pod-daemon through Cloud Hypervisor's hybrid vsock socket."""
    s = socket.socket(socket.AF_UNIX)
    s.settimeout(timeout)
    s.connect(vsock)
    s.sendall(f"CONNECT {port}\n".encode())
    line = b""
    while not line.endswith(b"\n"):
        c = s.recv(1)
        if not c:
            raise ConnectionError("vsock closed")
        line += c
    if not line.startswith(b"OK"):
        raise ConnectionError(line.decode(errors="replace"))
    payload = json.dumps(body).encode() if body is not None else b""
    s.sendall(f"{method} {path} HTTP/1.1\r\nHost: g\r\nConnection: close\r\nContent-Type: application/json\r\n"
              f"Content-Length: {len(payload)}\r\n\r\n".encode() + payload)
    buf = b""
    while b"\r\n\r\n" not in buf:
        c = s.recv(65536)
        if not c:
            break
        buf += c
    head, _, rest = buf.partition(b"\r\n\r\n")
    status = int(head.split()[1])
    low = head.lower()
    if b"content-length:" in low:
        n = int(low.split(b"content-length:")[1].split(b"\r\n")[0])
        while len(rest) < n:
            c = s.recv(65536)
            if not c:
                break
            rest += c
        rest = rest[:n]
    else:
        while True:
            c = s.recv(65536)
            if not c:
                break
            rest += c
    s.close()
    return status, rest.decode(errors="replace")


def healthy(vsock):
    try:
        return vsock_http(vsock, "GET", "/healthz")[0] == 200
    except Exception:
        return False


def guest(vsock, cmd, root=False, timeout=30):
    _, body = vsock_http(vsock, "POST", "/v1/exec", {"cmd": cmd, "as_root": root, "timeout_secs": timeout}, timeout=timeout + 5)
    try:
        return json.loads(body)
    except ValueError:
        return {"stdout": body, "stderr": "", "exit_code": -1}


def rss_mib(pid):
    for line in open(f"/proc/{pid}/status"):
        if line.startswith("VmRSS"):
            return int(line.split()[1]) / 1024
    return -1


def wait_for(pred, timeout, step=0.005):
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout:
        if pred():
            return time.monotonic() - t0
        time.sleep(step)
    raise TimeoutError


def med(xs):
    return round(statistics.median(xs), 2)


def spawn_ch(wd):
    os.makedirs(wd, exist_ok=True)
    cmd = [args.ch, "--api-socket", f"{wd}/api.sock"]
    if ambient_ptrace:
        # userfaultfd, which on-demand restore needs, asks for CAP_SYS_PTRACE unless vm.unprivileged_userfaultfd=1.
        cmd = ["sudo", "setpriv", f"--reuid={os.getuid()}", f"--regid={os.getgid()}", "--init-groups",
               "--inh-caps=+sys_ptrace", "--ambient-caps=+sys_ptrace", "--bounding-set=-all,+sys_ptrace"] + cmd
    p = subprocess.Popen(cmd, stdout=open(f"{wd}/ch.log", "w"), stderr=subprocess.STDOUT)
    procs.append(p)
    return p


def kill(p):
    try:
        p.terminate()
        p.wait(5)
    except Exception:
        try:
            p.kill()
            p.wait(5)
        except Exception:
            pass


def vm_config(disk, vsock, cid, fs_sock=None):
    if disk.endswith(".qcow2"):
        d = {"path": disk, "image_type": "Qcow2", "backing_files": True}
    else:
        d = {"path": disk, "image_type": "Raw"}
    cfg = {"cpus": {"boot_vcpus": 1, "max_vcpus": 1}, "memory": {"size": mem_mib << 20},
           "payload": {"kernel": args.kernel, "cmdline": CMDLINE}, "disks": [d], "net": [{"tap": TAP}],
           "vsock": {"cid": cid, "socket": vsock},
           "serial": {"mode": "File", "file": os.path.dirname(vsock) + "/serial.log"}, "console": {"mode": "Off"}}
    if fs_sock:
        cfg["memory"]["shared"] = True
        cfg["fs"] = [{"tag": "workspace", "socket": fs_sock}]
    return cfg


def cold_boot(name, disk, cid=7001, fs_sock=None):
    wd = f"{args.workdir}/{name}"
    shutil.rmtree(wd, ignore_errors=True)
    t0 = time.monotonic()
    p = spawn_ch(wd)
    api_sock, vsock = f"{wd}/api.sock", f"{wd}/vsock.sock"
    wait_for(lambda: os.path.exists(api_sock), 5)
    st, b = ch_api(api_sock, "PUT", "vm.create", vm_config(disk, vsock, cid, fs_sock))
    assert st in (200, 204), (st, b)
    st, b = ch_api(api_sock, "PUT", "vm.boot")
    assert st in (200, 204), (st, b)
    wait_for(lambda: healthy(vsock), 60, 0.02)
    return {"p": p, "api": api_sock, "vsock": vsock, "total": time.monotonic() - t0}


def restore(name, snap, mode):
    wd = f"{args.workdir}/{name}"
    shutil.rmtree(wd, ignore_errors=True)
    t0 = time.monotonic()
    p = spawn_ch(wd)
    api_sock = f"{wd}/api.sock"
    vsock = json.load(open(f"{snap}/config.json"))["vsock"]["socket"]  # the restored VM listens where the config says
    if os.path.exists(vsock):
        os.unlink(vsock)
    wait_for(lambda: os.path.exists(api_sock), 5)
    t1 = time.monotonic()
    body = {"source_url": "file://" + snap, "resume": True, "memory_restore_mode": {"copy": "Copy", "ondemand": "OnDemand"}[mode]}
    st, b = ch_api(api_sock, "PUT", "vm.restore", body)
    t2 = time.monotonic()
    if st not in (200, 204):
        return {"p": p, "error": f"vm.restore {st}: {b[:200]}"}
    wait_for(lambda: healthy(vsock), 60, 0.02)
    return {"p": p, "api": api_sock, "vsock": vsock, "call": t2 - t1, "total": time.monotonic() - t0}


def evict(path):
    """Drop one file from the page cache (the whole cache is not ours to drop)."""
    fd = os.open(path, os.O_RDONLY)
    os.fsync(fd)
    os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
    os.close(fd)


def copy_sparse(src, dst):
    sh("cp", "--sparse=always", src, dst)


def overlay(base, dst):
    sh("qemu-img", "create", "-q", "-f", "qcow2", "-b", base, "-F", "raw", dst)


def patch_helper(image):
    """Put the workspace helper of this checkout into a copy of the guest image."""
    mnt = f"{args.workdir}/mnt"
    os.makedirs(mnt, exist_ok=True)
    sh("sudo", "mount", "-o", "loop", image, mnt)
    try:
        target = f"{mnt}/usr/local/share/asp/mount-virtiofs-workspace.sh"
        sh("sudo", "cp", args.helper, target)
        sh("sudo", "chmod", "755", target)
    finally:
        sh("sudo", "umount", mnt)
    os.sync()


def virtiofsd(sock, shared):
    if os.path.exists(sock):
        os.unlink(sock)
    p = subprocess.Popen([args.virtiofsd, f"--socket-path={sock}", f"--shared-dir={shared}", "--sandbox=none", "--cache=auto"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    procs.append(p)
    wait_for(lambda: os.path.exists(sock), 5)
    return p


def section_cold():
    print("\n## Cold start: Cloud Hypervisor spawned until the pod-daemon answers\n")
    base_orig, base_new = f"{args.workdir}/base-orig.img", f"{args.workdir}/base-new.img"
    copy_sparse(args.image, base_orig)
    copy_sparse(args.image, base_new)
    patch_helper(base_new)
    # a loop mount and unmount leaves the original in the same clean state, so the two bases differ only in the helper
    mnt = f"{args.workdir}/mnt"
    sh("sudo", "mount", "-o", "loop", base_orig, mnt)
    sh("sudo", "umount", mnt)
    os.sync()
    ws = f"{args.workdir}/ws"
    os.makedirs(ws, exist_ok=True)
    open(f"{ws}/hello.txt", "w").write("hi\n")

    rows = []
    def run(label, make_disk, workspace=False):
        boots, prep = [], []
        for i in range(args.runs):
            t = time.monotonic()
            disk = make_disk(i)
            prep.append(time.monotonic() - t)
            fs_sock = vfs = None
            if workspace:
                fs_sock = f"{args.workdir}/vfs{i}.sock"
                vfs = virtiofsd(fs_sock, ws)
            r = cold_boot(f"cold{len(rows)}-{i}", disk, fs_sock=fs_sock)
            if workspace and "hi" not in guest(r["vsock"], ["sh", "-c", "cat /workspace/hello.txt"])["stdout"]:
                print("  !! the workspace was not mounted when the pod-daemon answered")
            boots.append(r["total"])
            kill(r["p"])
            if vfs:
                kill(vfs)
            time.sleep(0.4)
        rows.append((label, med(prep), med(boots)))

    fresh = lambda i: (copy_sparse(args.image, f"{args.workdir}/f{len(rows)}-{i}.img"), f"{args.workdir}/f{len(rows)}-{i}.img")[1]
    def flushed(i):
        d = fresh(i)
        os.sync()
        return d
    def ov(base):
        def make(i):
            d = f"{args.workdir}/o{len(rows)}-{i}.qcow2"
            overlay(base, d)
            return d
        return make

    run("a. a fresh sparse copy of the image, booted at once (what a node does today)", fresh)
    run("b. the copy flushed to disk before the boot", flushed)
    run("c. a qcow2 overlay on the image, no copy (helper as in main)", ov(base_orig))
    run("d. a qcow2 overlay on an image whose helper does not wait for an absent tag", ov(base_new))
    run("e. as c, with a workspace (virtiofs) attached", ov(base_orig), workspace=True)
    run("f. as d, with a workspace attached", ov(base_new), workspace=True)

    print("| Set-up | Preparing the disk | Boot to pod-daemon | Together |")
    print("|---|---|---|---|")
    for label, p, b in rows:
        print(f"| {label} | {p} s | {b} s | **{round(p + b, 2)} s** |")
    d = f"{args.workdir}/o3-0.qcow2"
    print(f"\nAn overlay holds {round(os.stat(d).st_blocks * 512 / 1048576, 1)} MiB after a boot; the copy it replaces is the size of the used part of the image.")


def section_snapshot():
    print("\n## Snapshot and restore of a 512 MiB guest\n")
    global mem_mib, ambient_ptrace
    mem_mib = 512
    ws = f"{args.workdir}/ws"
    os.makedirs(ws, exist_ok=True)
    open(f"{ws}/hello.txt", "w").write("hi\n")
    base = f"{args.workdir}/snap-base.img"
    copy_sparse(args.image, base)
    sh("sudo", "mount", "-o", "loop", base, f"{args.workdir}/mnt")
    sh("sudo", "umount", f"{args.workdir}/mnt")
    os.sync()
    disk = f"{args.workdir}/s.qcow2"
    overlay(base, disk)
    vm = cold_boot("s", disk, 7101)
    time.sleep(15)
    guest(vm["vsock"], ["sh", "-c", "setsid -f sleep 123456 </dev/null >/dev/null 2>&1; echo marker > /tmp/marker"], root=True)
    boot_id = guest(vm["vsock"], ["cat", "/proc/sys/kernel/random/boot_id"])["stdout"].strip()
    print(f"A running idle guest: Cloud Hypervisor holds {round(rss_mib(vm['p'].pid))} MiB of host memory.")
    snap = f"{args.workdir}/snap"
    os.makedirs(snap)
    t = time.monotonic()
    ch_api(vm["api"], "PUT", "vm.pause")
    t_pause = time.monotonic() - t
    t = time.monotonic()
    st, b = ch_api(vm["api"], "PUT", "vm.snapshot", {"destination_url": "file://" + snap})
    t_snap = time.monotonic() - t
    kill(vm["p"])
    size = os.stat(f"{snap}/memory-ranges")
    print(f"Pause {t_pause * 1000:.0f} ms, snapshot {t_snap:.2f} s ({st}). memory-ranges: {size.st_size // 1048576} MiB, "
          f"{size.st_blocks * 512 // 1048576} MiB allocated on disk (it is not sparse); config.json and state.json are small.")
    shutil.copy2(disk, f"{disk}.at-snapshot")
    time.sleep(20)  # the host moves on; the guest does not know
    r = restore("s-restore", snap, "copy")
    out = guest(r["vsock"], ["sh", "-c", "echo $(date +%s) $(cat /tmp/marker); grep -l sleep /proc/[0-9]*/comm | head -1; cat /proc/sys/kernel/random/boot_id"])
    clock, *rest = out["stdout"].split("\n")
    print(f"Restored by copy: the call took {r['call']:.2f} s and the guest answered {r['total']:.2f} s after the spawn. "
          f"The `sleep` it was running is still there: {bool(rest and 'comm' in rest[0])}; the marker file: {'marker' in clock}; "
          f"boot_id unchanged: {boot_id in out['stdout']}; its clock is {int(clock.split()[0]) - int(time.time())} s off after 20 s away.")
    print(f"Host memory after the restore: {round(rss_mib(r['p'].pid))} MiB (the guest's RAM is copied in whole).")
    kill(r["p"])

    shutil.copy2(f"{disk}.at-snapshot", disk)
    ambient_ptrace = False
    od = restore("s-od-plain", snap, "ondemand")
    print("On-demand restore (userfaultfd) without capabilities:", od.get("error") or "worked")
    kill(od["p"])
    print("  (/dev/userfaultfd exists:", os.path.exists("/dev/userfaultfd"), "| vm.unprivileged_userfaultfd =",
          open("/proc/sys/vm/unprivileged_userfaultfd").read().strip() + ")")

    print("\nA guest with a virtiofs workspace and files open on it at the snapshot, restored with a new virtiofsd:")
    fs_sock = f"{args.workdir}/vfs.sock"
    vfs = virtiofsd(fs_sock, ws)
    fdisk = f"{args.workdir}/fs.qcow2"
    overlay(base, fdisk)
    v = cold_boot("fs", fdisk, 7102, fs_sock=fs_sock)
    guest(v["vsock"], ["sh", "-c", "setsid -f sh -c 'exec 3>>/workspace/open.log; while :; do echo tick >&3; sleep 1; done' </dev/null >/dev/null 2>&1; "
                                   "setsid -f sh -c 'tail -f /workspace/hello.txt > /tmp/tail.out 2>&1' </dev/null >/dev/null 2>&1"], root=True)
    time.sleep(4)
    before = len(open(f"{ws}/open.log").read().split())
    fsnap = f"{args.workdir}/fsnap"
    os.makedirs(fsnap)
    ch_api(v["api"], "PUT", "vm.pause")
    st, b = ch_api(v["api"], "PUT", "vm.snapshot", {"destination_url": "file://" + fsnap})
    kill(v["p"])
    kill(vfs)
    time.sleep(2)
    vfs = virtiofsd(fs_sock, ws)
    r = restore("fs-restore", fsnap, "copy")
    if "error" in r:
        print("  restore failed:", r["error"])
    else:
        time.sleep(5)
        after = len(open(f"{ws}/open.log").read().split())
        out = guest(r["vsock"], ["sh", "-c", "echo appended >> /workspace/hello.txt; sleep 1; cat /tmp/tail.out"], root=True)
        print(f"  snapshot {st}, restored in {r['total']:.2f} s; the descriptor open on the share kept writing ({before} -> {after} lines), "
              f"and `tail -f` saw the new line: {'appended' in out['stdout']}. (One simple case, not a proof for a busy tree.)")
        kill(r["p"])
    kill(vfs)


def section_big():
    global mem_mib, ambient_ptrace
    base = f"{args.workdir}/big-base.img"
    copy_sparse(args.image, base)
    sh("sudo", "mount", "-o", "loop", base, f"{args.workdir}/mnt")
    sh("sudo", "umount", f"{args.workdir}/mnt")
    os.sync()
    print("\n## Bigger guests: the snapshot in and out of the page cache\n")
    print("| Guest RAM | Snapshot | memory-ranges | Restore by copy, file cached | by copy, file evicted | on demand, file evicted |")
    print("|---|---|---|---|---|---|")
    for mem in (2048, 4096):
        mem_mib = mem
        disk = f"{args.workdir}/b{mem}.qcow2"
        overlay(base, disk)
        vm = cold_boot(f"b{mem}", disk, 7200 + mem // 1024)
        time.sleep(12)
        snap = f"{args.workdir}/bsn{mem}"
        os.makedirs(snap)
        ch_api(vm["api"], "PUT", "vm.pause")
        t = time.monotonic()
        ch_api(vm["api"], "PUT", "vm.snapshot", {"destination_url": "file://" + snap}, timeout=600)
        t_snap = time.monotonic() - t
        size = os.stat(f"{snap}/memory-ranges")
        kill(vm["p"])
        shutil.copy2(disk, f"{disk}.at-snapshot")
        cells = []
        for cap, cold, mode in ((False, False, "copy"), (False, True, "copy"), (True, True, "ondemand")):
            ambient_ptrace = cap
            if cold:
                evict(f"{snap}/memory-ranges")
            shutil.copy2(f"{disk}.at-snapshot", disk)
            r = restore(f"br{mem}", snap, mode)
            cells.append(r.get("error", "") or f"{r['total']:.1f} s")
            kill(r["p"])
            time.sleep(0.5)
        ambient_ptrace = False
        print(f"| {mem // 1024} GiB | {t_snap:.1f} s | {size.st_blocks * 512 // 1048576} MiB | " + " | ".join(cells) + " |")
        shutil.rmtree(snap, ignore_errors=True)
    mem_mib = 512


def main():
    global args
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("sections", nargs="*", help="cold, snapshot, big (default: cold and snapshot)")
    ap.add_argument("--ch", default=shutil.which("cloud-hypervisor") or "/usr/local/bin/cloud-hypervisor")
    ap.add_argument("--virtiofsd", default=shutil.which("virtiofsd") or "/usr/local/bin/virtiofsd")
    ap.add_argument("--kernel", default="/opt/sandbox/vmlinux", help="the guest kernel (default: where asp image pull links it)")
    ap.add_argument("--image", default="/opt/sandbox/rootfs.img", help="the guest image")
    ap.add_argument("--helper", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "images/guest/helpers/mount-virtiofs-workspace.sh"),
                    help="the workspace helper to put into the image for the set-ups that use it")
    ap.add_argument("--workdir", default=os.path.expanduser("~/asp-bench"), help="where the throwaway disks and sockets go")
    ap.add_argument("--runs", type=int, default=4, help="boots per set-up")
    args = ap.parse_args()
    args.helper = os.path.abspath(args.helper)
    sections = args.sections or ["cold", "snapshot"]
    for name in sections:
        if name not in ("cold", "snapshot", "big"):
            ap.error(f"unknown section {name!r}: cold, snapshot or big")
    shutil.rmtree(args.workdir, ignore_errors=True)
    os.makedirs(args.workdir)
    print(f"host {sh('uname', '-r').stdout.strip()}, {sh(args.ch, '--version').stdout.splitlines()[0]}, "
          f"image {os.path.realpath(args.image)} ({os.stat(args.image).st_size // 1048576} MiB apparent), 1 vCPU, {args.runs} boots per set-up")
    sh("sudo", "ip", "tuntap", "add", "dev", TAP, "mode", "tap", "user", str(os.getuid()))
    sh("sudo", "ip", "addr", "add", f"{HOST_IP}/30", "dev", TAP)
    sh("sudo", "ip", "link", "set", TAP, "up")
    for name in sections:
        {"cold": section_cold, "snapshot": section_snapshot, "big": section_big}[name]()


def cleanup():
    for p in procs:
        kill(p)
    sh("sudo", "ip", "link", "del", TAP, check=False)
    if args:
        shutil.rmtree(args.workdir, ignore_errors=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        traceback.print_exc()
        sys.exit(1)
    finally:
        cleanup()
