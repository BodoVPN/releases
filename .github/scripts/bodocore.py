#!/usr/bin/env python3
"""Helpers for bodocore.yml: resolve the pinned libXray, build the Apple xcframework, smoke-test
every library through its real C or Swift entry points, and write the release's manifest.json,
SHA256SUMS and NOTES.md. Reuses core.py's binary checks.

Standard library only, so it runs the same on the Linux, Windows and macOS runners.
"""

import argparse
import contextlib
import ctypes
import datetime
import http.client
import http.server
import io
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import core  # noqa: E402  (core.yml's helpers: binary checks, zip, xcframework)

fail, run, write_json, sha256 = core.fail, core.run, core.write_json, core.sha256

MODULE = "github.com/bodovpn/releases/core"
LIBXRAY_MODULE = "github.com/xtls/libxray"
RELEASE_FLAG = f"{MODULE}/engine.release"
TARGETS = ["android", "apple", "linux-x64", "windows-x64"]
EXPORTS = ["CGoInvoke", "CGoFree", "BodoPingBatchWarm"]
AAR_CLASSES = ["com/bodovpn/bodocore/Bodocore.class", "com/bodovpn/bodocore/PingListener.class",
               "com/bodovpn/bodocore/Protector.class"]
MIRROR_TAG = re.compile(r"^v1\.(\d{2})(\d{2})(\d{2})\.(\d+)$")
APPLE_SLICES = [
    # (go os, go arch, apple arch, sdk, minimum OS, extra build tags)
    ("ios", "arm64", "arm64", "iphoneos", "15.0", "ios"),
    ("ios", "amd64", "x86_64", "iphonesimulator", "15.0", "ios"),
    ("ios", "arm64", "arm64", "iphonesimulator", "15.0", "ios"),
    ("darwin", "amd64", "x86_64", "macosx", "12.0", ""),
    ("darwin", "arm64", "arm64", "macosx", "12.0", ""),
]
MODULE_MAP = 'module BodoCore {\n  umbrella header "libBodoCore.h"\n  export *\n  module * { export * }\n}\n'


def go_modules(src):
    out = run(["go", "list", "-m", "-json", LIBXRAY_MODULE, core.XRAY_CORE_MODULE], cwd=src)
    decoder, modules, at = json.JSONDecoder(), {}, 0
    while at < len(out):
        module, at = decoder.raw_decode(out, at)
        modules[module["Path"]] = module
        while at < len(out) and out[at].isspace():
            at += 1
    return modules[LIBXRAY_MODULE], modules[core.XRAY_CORE_MODULE]


def cmd_resolve(args):
    """Pin check: go.mod's libXray mirror tag must be the same commit as its CalVer tag."""
    libxray, xray = go_modules(args.src)
    match = MIRROR_TAG.match(libxray["Version"])
    if not match:
        fail(f"{LIBXRAY_MODULE} {libxray['Version']} is not a v1.YYMMDD.N mirror tag")
    yy, mm, dd, _ = match.groups()
    calver = f"v{int(yy)}.{int(mm)}.{int(dd)}"
    mirror = core.ls_remote_tag(core.LIBXRAY_URL, libxray["Version"])
    tagged = core.ls_remote_tag(core.LIBXRAY_URL, calver)
    if not mirror or mirror != tagged:
        fail(f"libXray {libxray['Version']} ({mirror}) is not the {calver} release ({tagged})")
    if args.build and not re.fullmatch(r"[1-9][0-9]*", args.build):
        fail(f"build must be a positive whole number, got {args.build!r}")
    release = f"core-{calver}-bodo.{args.build or 'dev'}"
    outputs = {
        "release": release,
        "libxray_tag": calver,
        "libxray_version": libxray["Version"],
        "libxray_commit": mirror,
        "xray_module_version": xray["Version"],
        "xray_version": core.core_version(xray["Dir"]),
    }
    print(json.dumps(outputs, indent=2))
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as out:
        out.writelines(f"{key}={value}\n" for key, value in outputs.items())


def cmd_licenses(args):
    """libXray's MIT and Xray-core's MPL-2.0 licence, from the module cache, into a release dir."""
    libxray, xray = go_modules(args.src)
    for module, name in ((libxray, "libXray-LICENSE.txt"), (xray, "Xray-core-LICENSE.txt")):
        source = Path(module["Dir"]) / "LICENSE"
        if not source.is_file():
            fail(f"{module['Path']} {module['Version']} has no LICENSE")
        shutil.copyfile(source, Path(args.dir) / name)


class Site(http.server.ThreadingHTTPServer):
    """A loopback HTTP/1.1 server for the smoke tests that counts connections and requests."""

    daemon_threads = True

    def __init__(self):
        self.connections = 0
        self.requests = 0
        super().__init__(("127.0.0.1", 0), SiteHandler)

    def url(self):
        return f"http://127.0.0.1:{self.server_address[1]}/generate_204"


class SiteHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        self.server.connections += 1

    def answer(self):
        self.server.requests += 1
        self.send_response(204)
        self.end_headers()

    do_GET = do_HEAD = answer

    def log_message(self, *_):
        pass


@contextlib.contextmanager
def site():
    server = Site()
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()


def free_port():
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def request(method, payload=None):
    body = {"apiVersion": 3, "method": method}
    if payload is not None:
        body["payload"] = payload
    return json.dumps(body)


def ping_payload(url):
    return json.dumps({
        "configs": [
            {"xrayJson": json.dumps({"outbounds": [{"tag": "proxy", "protocol": "freedom"}]})},
            {"xrayJson": json.dumps({"outbounds": [{"tag": "proxy", "protocol": "blackhole"}]})},
        ],
        "url": url,
        "timeoutMs": 3000,
    })


def stats_config(listen_port, site_port):
    return json.dumps({
        "log": {"loglevel": "warning"},
        "inbounds": [{"listen": "127.0.0.1", "port": listen_port, "protocol": "dokodemo-door",
                      "settings": {"address": "127.0.0.1", "port": site_port, "network": "tcp"}}],
        "outbounds": [{"tag": "proxy", "protocol": "freedom"}],
        "stats": {},
        "policy": {"system": {"statsOutboundUplink": True, "statsOutboundDownlink": True}},
    })


def ok_data(text, label):
    try:
        response = json.loads(text)
    except ValueError:
        fail(f"{label}: not JSON: {text!r}")
    if not response.get("success"):
        fail(f"{label} failed: {text}")
    return response["data"]


class Smoke:
    """The checks every library passes through its own entry points; `call` sends one request."""

    def __init__(self, label, call, ping, expect):
        self.label, self.call, self.ping, self.expect = label, call, ping, expect
        self.record = {}

    def run(self):
        info = ok_data(self.call(request("buildInfo")), f"{self.label} buildInfo")
        wanted = {"release": self.expect["release"], "xray": self.expect["xray"], "libXray": self.expect["libxray"]}
        if any(info.get(key) != value for key, value in wanted.items()):
            fail(f"{self.label} buildInfo {info} does not match {wanted}")
        version = ok_data(self.call(request("xrayVersion")), f"{self.label} xrayVersion")["version"]
        if version != self.expect["xray"]:
            fail(f"{self.label} xrayVersion {version}, expected {self.expect['xray']}")
        self.record["buildInfo"] = info
        self.warm_ping()
        self.in_process_stats()
        print(f"{self.label}: {json.dumps(self.record)}")
        return self.record

    def warm_ping(self):
        with site() as server:
            rows, final = self.ping(ping_payload(server.url()))
            results = ok_data(final, f"{self.label} pingBatchWarm")["results"]
            seen = (server.connections, server.requests)
        if sorted(rows) != [0, 1]:
            fail(f"{self.label}: the callback streamed rows {sorted(rows)}, expected 0 and 1")
        good, bad = results
        if not (good["success"] and good["warm"] and good["delay"] >= 1) or bad["success"] or bad["delay"] != -1:
            fail(f"{self.label}: pingBatchWarm results {results}")
        if rows[0] != good or rows[1] != bad:
            fail(f"{self.label}: streamed {rows} but returned {results}")
        if seen != (1, 2):
            fail(f"{self.label}: the site saw {seen[0]} connections and {seen[1]} requests, expected 1 and 2")
        self.record["pingBatchWarm"] = {"results": results, "siteConnections": seen[0], "siteRequests": seen[1]}

    def in_process_stats(self):
        with site() as server:
            port = free_port()
            config = stats_config(port, server.server_address[1])
            ok_data(self.call(request("runXray", {"xrayJson": config})), f"{self.label} runXray")
            try:
                connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
                # Linux splices raw TCP to raw TCP and counts the spliced bytes when the
                # connection ends, so this one ends right after the reply.
                connection.request("GET", "/through-the-core", headers={"Connection": "close"})
                status = connection.getresponse().status
                connection.close()
                counters, proxy = {}, {}
                for _ in range(50):
                    counters = ok_data(self.call(request("queryStats")), f"{self.label} queryStats")
                    proxy = counters.get("outbound", {}).get("proxy", {})
                    if proxy.get("uplink", 0) > 0 and proxy.get("downlink", 0) > 0:
                        break
                    time.sleep(0.1)
            finally:
                ok_data(self.call(request("stopXray")), f"{self.label} stopXray")
        if status != 204 or proxy.get("uplink", 0) <= 0 or proxy.get("downlink", 0) <= 0:
            fail(f"{self.label}: GET through the core gave {status}; counters after 5 s {counters}")
        if ok_data(self.call(request("getXrayState")), f"{self.label} getXrayState")["running"]:
            fail(f"{self.label}: a core still runs after stopXray")
        self.record["queryStats"] = proxy


PING_CALLBACK = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_int32, ctypes.c_char_p)


def ctypes_smoke(path, expect):
    lib = ctypes.CDLL(str(path.resolve()))
    lib.CGoInvoke.restype = ctypes.c_void_p
    lib.CGoInvoke.argtypes = [ctypes.c_char_p]
    lib.CGoFree.argtypes = [ctypes.c_void_p]
    lib.BodoPingBatchWarm.restype = ctypes.c_void_p
    lib.BodoPingBatchWarm.argtypes = [ctypes.c_char_p, PING_CALLBACK, ctypes.c_void_p]

    def take(pointer):
        if not pointer:
            fail(f"{path.name} returned NULL")
        text = ctypes.string_at(pointer).decode("utf-8")
        lib.CGoFree(pointer)
        return text

    def call(text):
        return take(lib.CGoInvoke(text.encode()))

    def ping(payload):
        rows, lock = {}, threading.Lock()

        def on_result(_context, index, result_json):
            with lock:
                rows[index] = json.loads(result_json.decode("utf-8"))

        callback = PING_CALLBACK(on_result)
        final = take(lib.BodoPingBatchWarm(payload.encode(), callback, None))
        return rows, final

    return Smoke(path.name, call, ping, expect).run()


def expected(args):
    return {"release": args.release, "xray": args.xray, "libxray": args.libxray}


def header_check(header):
    text = header.read_text(encoding="utf-8")
    missing = [s for s in EXPORTS + ["bodo_ping_result_fn"] if s not in text]
    if missing:
        fail(f"{header.name} does not declare {', '.join(missing)}")


def checked_binary(path, machine):
    if not path.is_file() or path.stat().st_size == 0:
        fail(f"missing {path}")
    info = core.binary_machine(path)
    if info["machine"] != machine:
        fail(f"{path.name} is {info['machine']}, expected {machine}")
    if info.get("stripped") is False:
        fail(f"{path.name} keeps {', '.join(info['debugSections'])}: it was built without -s -w")
    info["moduleReplacePaths"] = core.check_build_paths(path.name, path.read_bytes())
    return info


def cmd_desktop(args):
    root = Path(args.dir)
    lib, exe, header = root / args.lib, root / args.exe, root / "libBodoCore.h"
    if not header.is_file():
        fail(f"missing {header}")
    header_check(header)
    binaries = {args.lib: checked_binary(lib, args.machine), args.exe: checked_binary(exe, args.machine)}
    exported = core.exports(lib, EXPORTS)
    smoke = ctypes_smoke(lib, expected(args))
    exe_smoke = executable_smoke(exe)
    write_json(args.out, {"binaries": binaries, "libraryExports": exported, "smoke": smoke, "executable": exe_smoke})


def uplink_interface():
    """The default route's interface name, as Go's net.InterfaceByName reads it."""
    if os.name == "nt":
        name = run(["powershell", "-NoProfile", "-Command",
                    "(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Sort-Object RouteMetric | "
                    "Select-Object -First 1).InterfaceAlias"])
    else:
        match = re.search(r"\bdev (\S+)", run(["ip", "route", "get", "8.8.8.8"]))
        name = match.group(1) if match else ""
    if not name:
        fail("found no uplink interface for -interface")
    return name


def read_lines(process, prefix, until, timeout):
    """Lines from process's stdout that start with prefix, parsed, until until(rows) or timeout."""
    rows, deadline = [], time.monotonic() + timeout
    found = queue_lines(process)
    while time.monotonic() < deadline and not until(rows):
        try:
            line = found.get(timeout=0.5)
        except Exception:  # queue.Empty
            if process.poll() is not None and found.empty():
                break
            continue
        if line is None:
            break
        if line.startswith(prefix):
            rows.append(json.loads(line[len(prefix):]))
    return rows


def queue_lines(process):
    import queue

    lines = queue.Queue()

    def pump():
        for line in process.stdout:
            lines.put(line.rstrip("\r\n"))
        lines.put(None)

    threading.Thread(target=pump, daemon=True).start()
    return lines


def executable_smoke(exe):
    """bodocore through its command line: usage, refusal, a streamed warm ping, and run's counters."""
    binary = str(exe.resolve())
    usage = subprocess.run([binary, "-h"], capture_output=True, text=True, timeout=30)
    if usage.returncode != 0 or not usage.stdout.startswith("Usage: bodocore run") or "bodocore ping" not in usage.stdout:
        fail(f"{exe.name} -h: exit {usage.returncode}, {usage.stdout!r}")
    if subprocess.run([binary], capture_output=True, timeout=30).returncode == 0:
        fail(f"{exe.name} without arguments exited 0")
    iface = uplink_interface()
    bind = ["-dns", "8.8.8.8:53", "-interface", iface]
    record = {"usage": usage.stdout.splitlines()[0], "interface": iface}

    with site() as server:
        process = subprocess.Popen([binary, "ping", *bind], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        process.stdin.write(ping_payload(server.url()))
        process.stdin.close()
        rows = read_lines(process, "bodocore-ping ", lambda r: len(r) >= 2, 30)
        code, stderr = process.wait(timeout=30), process.stderr.read()
        seen = (server.connections, server.requests)
    by_index = {row["index"]: row for row in rows}
    good, bad = by_index.get(0, {}), by_index.get(1, {})
    if code != 0 or not (good.get("success") and good.get("warm")) or bad.get("success", True) or seen != (1, 2):
        fail(f"{exe.name} ping: exit {code}, rows {rows}, site {seen}, stderr {stderr!r}")
    record["ping"] = {"rows": rows, "siteConnections": seen[0], "siteRequests": seen[1]}

    with site() as server:
        port = free_port()
        work = Path(os.environ.get("RUNNER_TEMP", ".")) / "bodocore-run"
        work.mkdir(exist_ok=True)
        config = work / "session.json"
        config.write_text(stats_config(port, server.server_address[1]), encoding="utf-8")
        process = subprocess.Popen([binary, "run", *bind, "-config", str(config), "-stats-interval", "200ms"],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            for _ in range(50):
                with contextlib.suppress(OSError), socket.create_connection(("127.0.0.1", port), timeout=1):
                    break
                time.sleep(0.1)
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
            connection.request("GET", "/through-the-core", headers={"Connection": "close"})
            status = connection.getresponse().status
            connection.close()

            def counted(rows):
                proxy = rows[-1].get("outbound", {}).get("proxy", {}) if rows else {}
                return proxy.get("uplink", 0) > 0 and proxy.get("downlink", 0) > 0

            stats = read_lines(process, "bodocore-stats ", counted, 15)
        finally:
            process.terminate()
            process.wait(timeout=30)
    if status != 204 or not stats or not counted(stats):
        fail(f"{exe.name} run: GET {status}, stats lines {stats[-3:]}, stderr {process.stderr.read()!r}")
    record["run"] = {"statsLines": len(stats), "proxy": stats[-1]["outbound"]["proxy"]}
    print(f"{exe.name}: {json.dumps(record)}")
    return record


def cmd_aar(args):
    checked = Path(args.out).with_suffix(".core.json")
    core.cmd_aar(argparse.Namespace(aar=args.aar, out=str(checked)))
    contents = json.loads(checked.read_text(encoding="utf-8"))
    checked.unlink()
    with zipfile.ZipFile(args.aar) as archive:
        with zipfile.ZipFile(io.BytesIO(archive.read("classes.jar"))) as classes:
            names = set(classes.namelist())
        rules = archive.read("proguard.txt").decode("utf-8")
    missing = [name for name in AAR_CLASSES if name not in names]
    if missing:
        fail(f"classes.jar lacks {', '.join(missing)}")
    if "-keep class com.bodovpn.** { *; }" not in rules:
        fail(f"the AAR's proguard.txt does not keep com.bodovpn.**: {rules!r}")
    contents["api"] = sorted(name for name in names if name.startswith("com/bodovpn/") and "$" not in name)
    write_json(args.out, contents)


def cmd_apple_build(args):
    """c-archive per slice, lipo per SDK, one xcframework with headers in Headers/BodoCore/."""
    src, work = Path(args.src).resolve(), Path(args.work).resolve()
    shutil.rmtree(work, ignore_errors=True)
    by_sdk = {}
    for goos, goarch, arch, sdk, minimum, tags in APPLE_SLICES:
        sdk_path = run(["xcrun", "--sdk", sdk, "--show-sdk-path"])
        flags = f"-isysroot {sdk_path} -m{sdk}-version-min={minimum} -arch {arch}"
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="1",
                   CC=f"xcrun --sdk {sdk} --toolchain {sdk} clang",
                   CXX=f"xcrun --sdk {sdk} --toolchain {sdk} clang++",
                   CGO_CFLAGS=flags, CGO_CXXFLAGS=flags, CGO_LDFLAGS=f"{flags} -Wl,-Bsymbolic-functions")
        out = work / f"{sdk}-{arch}" / "libBodoCore.a"
        command = ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", args.ldflags,
                   "-buildmode=c-archive", f"-o={out}", "./cbridge"]
        if tags:
            command[2:2] = ["-tags", tags]
        print(" ".join(command), f"({goos}/{goarch}, {sdk})", flush=True)
        if subprocess.run(command, cwd=src, env=env).returncode != 0:
            fail(f"go build for {sdk} {arch} failed")
        by_sdk.setdefault(sdk, []).append(out)
    include = work / "include" / "BodoCore"
    include.mkdir(parents=True)
    shutil.copy(by_sdk["iphoneos"][0].with_suffix(".h"), include / "libBodoCore.h")
    (include / "module.modulemap").write_text(MODULE_MAP, encoding="utf-8")
    command = ["xcodebuild", "-create-xcframework"]
    for sdk, archives in by_sdk.items():
        library = archives[0]
        if len(archives) > 1:
            library = work / f"{sdk}-universal" / "libBodoCore.a"
            library.parent.mkdir()
            run(["lipo", "-create", *map(str, archives), "-output", str(library)])
        command += ["-library", str(library), "-headers", str(work / "include")]
    output = Path(args.output).resolve()
    shutil.rmtree(output, ignore_errors=True)
    output.parent.mkdir(parents=True, exist_ok=True)
    run(command + ["-output", str(output)])
    print(f"{output}: {', '.join(sorted(p.name for p in output.iterdir()))}")


def cmd_xcframework(args):
    checked = Path(args.out).with_suffix(".core.json")
    core.cmd_xcframework(argparse.Namespace(
        path=args.path, require="ios,ios-simulator,macos", max_minos="ios=15.0,macos=13.0",
        headers_subdir="BodoCore", out=str(checked)))
    contents = json.loads(checked.read_text(encoding="utf-8"))
    checked.unlink()
    root = Path(args.path)
    for ident in (s["identifier"] for s in contents["slices"]):
        library = root / ident / "libBodoCore.a"
        header_check(root / ident / "Headers" / "BodoCore" / "libBodoCore.h")
        if not re.search(r"\b_BodoPingBatchWarm$", run(["nm", "-gU", str(library)]), re.M):
            fail(f"{ident}: the archive does not define _BodoPingBatchWarm")
    write_json(args.out, contents)


SWIFTPM_PACKAGE = """// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "CoreSmoke",
    platforms: [.macOS("@PLATFORM@")],
    targets: [
        .binaryTarget(name: "BodoCore", path: "@ZIP@"),
        .executableTarget(name: "smoke", dependencies: ["BodoCore"], linkerSettings: [@LINKER@]),
    ]
)
"""
# One request per stdin line, one reply per stdout line; "PING <payload>" streams ROW lines first.
SWIFTPM_MAIN = """import BodoCore
import Foundation

setvbuf(stdout, nil, _IOLBF, 0)
let onRow: bodo_ping_result_fn = { _, index, json in
    print("ROW \\(index) \\(String(cString: json!))")
}
func take(_ reply: UnsafeMutablePointer<CChar>?) -> String {
    guard let reply else { fatalError("the core returned NULL") }
    defer { CGoFree(reply) }
    return String(cString: reply)
}
while let line = readLine() {
    if line.hasPrefix("PING ") {
        print("END " + take(BodoPingBatchWarm(strdup(String(line.dropFirst(5))), onRow, nil)))
    } else {
        print("END " + take(CGoInvoke(strdup(line))))
    }
}
"""


class SwiftClient:
    def __init__(self, binary):
        self.process = subprocess.Popen([str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)

    def exchange(self, line):
        self.process.stdin.write(line + "\n")
        self.process.stdin.flush()
        rows = {}
        while True:
            out = self.process.stdout.readline()
            if not out:
                fail(f"the Swift smoke binary exited ({self.process.wait()})")
            if out.startswith("ROW "):
                _, index, text = out.rstrip("\n").split(" ", 2)
                rows[int(index)] = json.loads(text)
            elif out.startswith("END "):
                return rows, out[4:].rstrip("\n")

    def close(self):
        self.process.stdin.close()
        if self.process.wait(timeout=30) != 0:
            fail(f"the Swift smoke binary exited {self.process.returncode}")


def cmd_swiftpm(args):
    """Consume the zip the way the app will: a SwiftPM binaryTarget, imported and run from Swift."""
    archive = Path(args.zip).resolve()
    work = Path(os.environ.get("RUNNER_TEMP", "/tmp")) / "bodocore-swiftpm"
    shutil.rmtree(work, ignore_errors=True)
    (work / "Sources" / "smoke").mkdir(parents=True)
    shutil.copy(archive, work / archive.name)
    (work / "Package.swift").write_text(
        SWIFTPM_PACKAGE.replace("@PLATFORM@", args.platform).replace("@ZIP@", archive.name)
        .replace("@LINKER@", ", ".join(core.SWIFTPM_LINKER_SETTINGS)), encoding="utf-8")
    (work / "Sources" / "smoke" / "main.swift").write_text(SWIFTPM_MAIN, encoding="utf-8")
    checksum = run(["swift", "package", "compute-checksum", archive.name], cwd=work)
    if checksum != sha256(archive):
        fail(f"swift package compute-checksum gave {checksum}, not the zip's sha256")
    build = subprocess.run(["swift", "build", "-c", "release"], cwd=work, capture_output=True, text=True)
    print(build.stdout + build.stderr)
    if build.returncode != 0:
        fail("the SwiftPM smoke package does not build against the xcframework")
    newer = [l.strip() for l in (build.stdout + build.stderr).splitlines() if "built for newer" in l]
    if newer:
        fail(f"the xcframework targets a newer macOS than {args.platform}: {newer[0]}")
    binary = Path(run(["swift", "build", "-c", "release", "--show-bin-path"], cwd=work)) / "smoke"
    client = SwiftClient(binary)
    smoke = Smoke("SwiftPM", lambda line: client.exchange(line)[1],
                  lambda payload: client.exchange("PING " + payload), expected(args)).run()
    client.close()
    write_json(args.out, {"checksum": checksum, "deploymentTarget": f"macOS {args.platform}",
                          "linkerSettings": core.SWIFTPM_LINKER_SETTINGS, "smoke": smoke})


def cmd_info(args):
    goversion = run(["go", "env", "GOVERSION"])
    if goversion != f"go{os.environ['GO_VERSION']}":
        fail(f"built with {goversion}, but the pin is go{os.environ['GO_VERSION']}")
    tools = {"go": goversion, "python": sys.version.split()[0]}
    for pair in args.tool or []:
        key, _, value = pair.partition("=")
        tools[key] = value.strip()
    contents = json.loads(Path(args.contents).read_text(encoding="utf-8"))
    for pair in args.merge or []:
        key, _, path = pair.partition("=")
        contents[key] = json.loads(Path(path).read_text(encoding="utf-8"))
    write_json(args.out, {
        "target": args.target,
        "asset": f"bodocore-{args.target}.zip",
        "runner": {"os": os.environ.get("ImageOS"), "image": os.environ.get("ImageVersion"),
                   "arch": os.environ.get("RUNNER_ARCH")},
        "libxrayCommit": os.environ["LIBXRAY_COMMIT"],
        "xrayVersion": os.environ["XRAY_VERSION"],
        "toolchain": tools,
        "buildFlags": args.flags,
        "contents": contents,
    })


def describe(asset):
    contents = asset["contents"]
    if "abis" in contents:
        return f"`BodoCore.aar` ({', '.join(contents['abis'])}; minSdk {contents['minSdk']}) + sources jar"
    if "slices" in contents:
        slices = ", ".join(s["identifier"] for s in contents["slices"])
        return f"`BodoCore.xcframework` (static): {slices}. SwiftPM `binaryTarget` checksum `{asset['swiftpmChecksum']}`"
    names = list(contents["binaries"])
    return f"`{names[0]}` + `libBodoCore.h`, and the desktop core `{names[1]}` (`run` with counters on stdout, `ping`)"


def cmd_manifest(args):
    dist, env = Path(args.dist), os.environ
    infos = {}
    for path in sorted(dist.glob("bodocore-*.json")):
        info = json.loads(path.read_text(encoding="utf-8"))
        infos[info["target"]] = info
    missing = sorted(set(TARGETS) - set(infos))
    if missing:
        fail(f"no build for {', '.join(missing)}")
    assets = []
    for target in TARGETS:
        info = infos[target]
        if info["libxrayCommit"] != env["LIBXRAY_COMMIT"] or info["xrayVersion"] != env["XRAY_VERSION"]:
            fail(f"{target} was built from libXray {info['libxrayCommit']} / Xray {info['xrayVersion']}")
        path = dist / info["asset"]
        if not path.is_file():
            fail(f"missing {path}")
        digest = sha256(path)
        swiftpm = info["contents"].get("swiftpm")
        if swiftpm and swiftpm["checksum"] != digest:
            fail(f"{target}: the SwiftPM checksum {swiftpm['checksum']} is not the zip's sha256 {digest}")
        assets.append({
            "name": info["asset"], "target": target, "size": path.stat().st_size, "sha256": digest,
            **({"swiftpmChecksum": swiftpm["checksum"]} if swiftpm else {}),
            "files": core.zip_listing(path), "runner": info["runner"], "toolchain": info["toolchain"],
            "buildFlags": info["buildFlags"], "contents": info["contents"],
        })
    manifest = {
        "schema": 1,
        "release": env["RELEASE"],
        "builtAt": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "build": {
            "repository": env["GITHUB_REPOSITORY"], "workflowCommit": env["GITHUB_SHA"], "ref": env["GITHUB_REF"],
            "run": f"{env['GITHUB_SERVER_URL']}/{env['GITHUB_REPOSITORY']}/actions/runs/{env['GITHUB_RUN_ID']}",
            "module": MODULE, "moduleDir": "core",
        },
        "libxray": {"repository": "XTLS/libXray", "tag": env["LIBXRAY_TAG"], "goModuleVersion": env["LIBXRAY_VERSION"],
                    "commit": env["LIBXRAY_COMMIT"], "license": "MIT"},
        "xrayCore": {"repository": "XTLS/Xray-core", "module": core.XRAY_CORE_MODULE, "version": env["XRAY_VERSION"],
                     "moduleVersion": env["XRAY_MODULE_VERSION"],
                     **core.resolve_xray_core(env["XRAY_MODULE_VERSION"], env["XRAY_VERSION"]), "license": "MPL-2.0"},
        "toolchain": {"go": env["GO_VERSION"], "gomobile": env["GOMOBILE_VERSION"], "ndk": env["NDK_VERSION"],
                      "ndkRevision": env["NDK_REVISION"], "androidPlatform": env["ANDROID_PLATFORM"],
                      "java": f"temurin {env['JAVA_VERSION']}", "xcode": env["XCODE_VERSION"],
                      "python": env["PYTHON_VERSION"]},
        "notes": [
            "One Go runtime per process: each library holds libXray and BodoVPN's additions (warm pingBatchWarm, "
            "in-process queryStats, Android protect). An app loads this library instead of libXray's, never both.",
            "Every native binary is checked stripped (-s -w) and free of build-machine paths (-trimpath); the "
            "one path left is gomobile's module replace in the Go build info (moduleReplacePaths).",
            "Every library is smoke-tested through its real entry points before release: ctypes on Linux and "
            "Windows, a SwiftPM executable on macOS (contents.smoke).",
            "The Windows and Linux zips also hold bodocore, the desktop core an app runs out of its own process: "
            "`bodocore run -dns -interface -config [-error-file] [-stats-interval]` (libXray's desktop `xray run`, plus "
            "`bodocore-stats <counters JSON>` lines on stdout) and `bodocore ping -dns -interface` (a PingRequest on "
            "stdin, one `bodocore-ping <row JSON>` line per row). Smoke-tested from its command line "
            "(contents.executable).",
            "The release files carry a Sigstore build-provenance attestation: "
            "gh attestation verify <file> -R BodoVPN/releases.",
        ],
        "assets": assets,
    }
    write_json(dist / "manifest.json", manifest)
    sums = [(a["sha256"], a["name"]) for a in assets] + [(sha256(dist / "manifest.json"), "manifest.json")]
    (dist / "SHA256SUMS").write_text("".join(f"{h}  {n}\n" for h, n in sorted(sums, key=lambda s: s[1])), encoding="utf-8")
    rows = "\n".join(f"| `{a['name']}` | {describe(a)} |" for a in assets)
    tool = manifest["toolchain"]
    (dist / "NOTES.md").write_text(
        f"BodoVPN's core: our Go module (`core/` in this repo at `{env['GITHUB_SHA'][:12]}`) built in one "
        f"bind with [XTLS/libXray {env['LIBXRAY_TAG']}]({core.LIBXRAY_URL}/tree/{env['LIBXRAY_COMMIT']}) (MIT) "
        f"and Xray-core {env['XRAY_VERSION']} (MPL-2.0). It adds a warm real-delay batch (`pingBatchWarm`), "
        "in-process traffic counters (`queryStats`) and Android's socket `protect` to libXray's API.\n\n"
        "This is a build input for the apps, not an app release. The app downloads are on the "
        f"[latest release](https://github.com/{env['GITHUB_REPOSITORY']}/releases/latest).\n\n"
        f"| Asset | Contents |\n| --- | --- |\n{rows}\n\n"
        f"Toolchain: Go {tool['go']}, gomobile {tool['gomobile']}, NDK {tool['ndk']} ({tool['ndkRevision']}), "
        f"{tool['androidPlatform']}, Java {tool['java']}, Xcode {tool['xcode']}.\n"
        "Every pinned version and each asset's details are in `manifest.json`; checksums are in `SHA256SUMS`. "
        f"Verify a download with `gh attestation verify <file> -R {env['GITHUB_REPOSITORY']}`.\n",
        encoding="utf-8")
    print((dist / "SHA256SUMS").read_text(encoding="utf-8"))


def add_expect(parser):
    parser.add_argument("--release", required=True)
    parser.add_argument("--xray", required=True)
    parser.add_argument("--libxray", required=True, help="libXray's Go module version, e.g. v1.260930.0")


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("resolve")
    p.add_argument("--src", required=True)
    p.add_argument("--build", default="")
    p.set_defaults(func=cmd_resolve)

    p = sub.add_parser("licenses")
    p.add_argument("dir")
    p.add_argument("--src", required=True)
    p.set_defaults(func=cmd_licenses)

    p = sub.add_parser("desktop")
    p.add_argument("dir")
    p.add_argument("--lib", required=True)
    p.add_argument("--exe", required=True)
    p.add_argument("--machine", required=True)
    p.add_argument("--out", required=True)
    add_expect(p)
    p.set_defaults(func=cmd_desktop)

    p = sub.add_parser("aar")
    p.add_argument("aar")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_aar)

    p = sub.add_parser("apple-build")
    p.add_argument("--src", required=True)
    p.add_argument("--work", required=True)
    p.add_argument("--ldflags", required=True)
    p.add_argument("--output", required=True)
    p.set_defaults(func=cmd_apple_build)

    p = sub.add_parser("xcframework")
    p.add_argument("path")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_xcframework)

    p = sub.add_parser("swiftpm")
    p.add_argument("zip")
    p.add_argument("--platform", required=True)
    p.add_argument("--out", required=True)
    add_expect(p)
    p.set_defaults(func=cmd_swiftpm)

    p = sub.add_parser("zip")
    p.add_argument("src")
    p.add_argument("out")
    p.add_argument("--flat", action="store_true")
    p.set_defaults(func=core.cmd_zip)

    p = sub.add_parser("info")
    p.add_argument("--target", required=True, choices=TARGETS)
    p.add_argument("--contents", required=True)
    p.add_argument("--flags", required=True)
    p.add_argument("--tool", action="append")
    p.add_argument("--merge", action="append", help="key=file: add a JSON file to contents under key")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_info)

    p = sub.add_parser("manifest")
    p.add_argument("dist")
    p.set_defaults(func=cmd_manifest)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
