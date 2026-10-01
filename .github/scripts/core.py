#!/usr/bin/env python3
"""Helpers for core.yml: resolve the libXray tag, smoke-check each build, zip it
deterministically, and write the release's manifest.json and SHA256SUMS.

Standard library only, so it runs the same on the Linux, Windows and macOS runners.
"""

import argparse
import datetime
import hashlib
import io
import json
import os
import plistlib
import re
import stat
import struct
import subprocess
import sys
import zipfile
from pathlib import Path

LIBXRAY_URL = "https://github.com/XTLS/libXray"
XRAY_CORE_URL = "https://github.com/XTLS/Xray-core"
XRAY_CORE_MODULE = "github.com/xtls/xray-core"
TAG_PATTERN = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+$")
ANDROID_ABIS = {"arm64-v8a": 0xB7, "armeabi-v7a": 0x28, "x86": 0x03, "x86_64": 0x3E}
ELF_MACHINES = {0x03: "x86", 0x28: "arm", 0x3E: "x86_64", 0xB7: "aarch64"}
PE_MACHINES = {0x8664: "x86_64", 0xAA64: "aarch64", 0x014C: "x86"}
PAGE_16K = 16384
USAGE_PREFIX = "Usage: xray run"
GOMOBILE_LDFLAGS = "-s -w -buildid="
TARGETS = ["android", "apple-cgo", "apple-gomobile", "linux-x64", "windows-x64"]
MANIFEST_NOTES = [
    "Every native binary is checked to be stripped (-s -w) and free of build-machine paths (-trimpath). "
    "libXray's cgo, Linux and Windows builds already pass these flags; its gomobile bind commands (android, "
    "apple gomobile) pass neither, so this build adds them there (assets[].buildPatch). Other clients' gomobile "
    "builds strip and trim the same way. The one path -trimpath can't remove is the checkout directory gomobile "
    "records as a module replace in the Go build info (moduleReplacePaths). See: "
    "https://github.com/2dust/AndroidLibXrayLite/blob/d0c6c4ae1b09c912070c8288bd0dbcc2e492ac29/.github/workflows/main.yml, "
    "https://github.com/KaringX/sing-box/blob/73603b201141ee9bab41af76995808ccfc45209f/cmd/internal/build_libbox/main.go",
    "The Apple zips hold LibXray.xcframework at their root, the layout a SwiftPM binaryTarget expects. "
    "libxray-apple-cgo.zip is built and run from a SwiftPM package in CI (assets[].contents.swiftpm): the "
    "importing target links CoreFoundation, Security and libresolv, which the Go runtime needs.",
    "The release files carry a Sigstore build-provenance attestation: "
    "gh attestation verify <file> -R BodoVPN/releases.",
]


def fail(message):
    print(f"::error::{message}", flush=True)
    sys.exit(1)


def run(cmd, cwd=None):
    result = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if result.returncode != 0:
        fail(f"{' '.join(cmd)} failed ({result.returncode}): {result.stderr.strip()}")
    return result.stdout.strip()


def write_json(path, data):
    Path(path).write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


def ls_remote_tag(url, tag):
    out = run(["git", "ls-remote", url, f"refs/tags/{tag}", f"refs/tags/{tag}^{{}}"])
    refs = {ref: sha for sha, ref in (line.split("\t") for line in out.splitlines() if line)}
    return refs.get(f"refs/tags/{tag}^{{}}") or refs.get(f"refs/tags/{tag}")


def cmd_resolve(args):
    tag = os.environ.get("LIBXRAY_TAG", "")
    if not TAG_PATTERN.match(tag):
        fail(f"libxray_tag must look like v26.9.30, got {tag!r}")
    commit = ls_remote_tag(LIBXRAY_URL, tag)
    if not commit or not re.fullmatch(r"[0-9a-f]{40}", commit):
        fail(f"tag {tag} does not resolve to a commit in XTLS/libXray")
    print(f"XTLS/libXray {tag} = {commit}")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as out:
        out.write(f"commit={commit}\n")


def cmd_patch_gomobile(args):
    path = Path(args.src) / "build" / "app" / args.script
    text = path.read_bytes().decode("utf-8")
    if text.count('"bind",') != 1:
        fail(f"{args.script}: expected one gomobile bind command")
    ldflags = re.findall(r'"-ldflags=([^"]*)"', text)
    if len(ldflags) > 1:
        fail(f"{args.script}: expected at most one -ldflags")
    added = []
    if '"-trimpath"' not in text:
        added.append('"-trimpath"')
    if ldflags:
        text = text.replace(f'"-ldflags={ldflags[0]}"', f'"-ldflags={GOMOBILE_LDFLAGS} {ldflags[0]}"')
        final = f"{GOMOBILE_LDFLAGS} {ldflags[0]}"
    else:
        added.append(f'"-ldflags={GOMOBILE_LDFLAGS}"')
        final = GOMOBILE_LDFLAGS
    if added:
        text = text.replace('"bind",', '"bind", ' + ", ".join(added) + ",")
    path.write_bytes(text.encode("utf-8"))
    print(f"{args.script}: gomobile bind -trimpath -ldflags='{final}'")
    write_json(args.out, {"script": f"build/app/{args.script}", "trimpath": True, "ldflags": final})


def zip_entries(src):
    for child in sorted(src.iterdir(), key=lambda p: p.name):
        yield child
        if child.is_dir() and not child.is_symlink():
            yield from zip_entries(child)


def is_executable(path):
    if os.name == "nt":
        return path.suffix.lower() in {".exe", ".dll"}
    return bool(path.stat().st_mode & 0o111)


def cmd_zip(args):
    src = Path(args.src)
    epoch = max(int(os.environ["SOURCE_DATE_EPOCH"]), 315532800)
    stamp = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).timetuple()[:6]

    def entry(name, mode):
        info = zipfile.ZipInfo(name, stamp)
        info.create_system = 3
        info.external_attr = mode << 16
        return info

    prefix = "" if args.flat else f"{src.name}/"
    with zipfile.ZipFile(args.out, "w") as archive:
        if prefix:
            archive.writestr(entry(prefix, stat.S_IFDIR | 0o755), b"")
        for path in zip_entries(src):
            name = f"{prefix}{path.relative_to(src).as_posix()}"
            if path.is_symlink():
                archive.writestr(entry(name, stat.S_IFLNK | 0o777), os.readlink(path))
            elif path.is_dir():
                archive.writestr(entry(f"{name}/", stat.S_IFDIR | 0o755), b"")
            else:
                info = entry(name, stat.S_IFREG | (0o755 if is_executable(path) else 0o644))
                info.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(info, path.read_bytes(), compresslevel=9)
    print(f"{args.out}: {Path(args.out).stat().st_size} bytes")


def elf_info(data, label):
    if data[:4] != b"\x7fELF":
        fail(f"{label} is not an ELF file")
    is64 = data[4] == 2
    machine = struct.unpack_from("<H", data, 18)[0]
    if is64:
        phoff, = struct.unpack_from("<Q", data, 0x20)
        phentsize, phnum = struct.unpack_from("<HH", data, 0x36)
    else:
        phoff, = struct.unpack_from("<I", data, 0x1C)
        phentsize, phnum = struct.unpack_from("<HH", data, 0x2A)
    load_aligns, interp = [], False
    for i in range(phnum):
        base = phoff + i * phentsize
        p_type, = struct.unpack_from("<I", data, base)
        p_align, = struct.unpack_from("<Q", data, base + 0x30) if is64 else struct.unpack_from("<I", data, base + 0x1C)
        if p_type == 1:
            load_aligns.append(p_align)
        elif p_type == 3:
            interp = True
    debug = sorted(n for n in elf_section_names(data, is64) if n == ".symtab" or n.startswith((".debug_", ".zdebug_")))
    return {
        "machine": ELF_MACHINES.get(machine, hex(machine)),
        "bits": 64 if is64 else 32,
        "loadAlign": min(load_aligns) if load_aligns else 0,
        "programInterpreter": interp,
        "stripped": not debug,
        "debugSections": debug,
    }


def elf_section_names(data, is64):
    if is64:
        shoff, = struct.unpack_from("<Q", data, 0x28)
        shentsize, shnum, shstrndx = struct.unpack_from("<HHH", data, 0x3A)
        layout = "<IIQQQQIIQQ"
    else:
        shoff, = struct.unpack_from("<I", data, 0x20)
        shentsize, shnum, shstrndx = struct.unpack_from("<HHH", data, 0x2E)
        layout = "<IIIIIIIIII"
    if shoff == 0 or shnum == 0:
        return []
    sections = [struct.unpack_from(layout, data, shoff + i * shentsize) for i in range(shnum)]
    strtab = sections[shstrndx][4]
    return [data[strtab + s[0]:data.index(b"\0", strtab + s[0])].decode() for s in sections]


def build_path_prefixes():
    values = {os.environ.get(v, "") for v in ("GITHUB_WORKSPACE", "RUNNER_TOOL_CACHE", "RUNNER_TEMP", "HOME", "USERPROFILE")}
    values |= set(run(["go", "env", "GOROOT", "GOPATH", "GOMODCACHE"]).splitlines())
    prefixes = set()
    for value in values:
        value = value.strip().rstrip("/\\")
        if len(value) > 4:
            prefixes |= {value, value.replace("\\", "/"), value.replace("/", "\\")}
    return sorted(p.encode() for p in prefixes)


PATH_PREFIXES = []


MODINFO_REPLACE = re.compile(rb"\n=>\t([^\t\n]+)\t")


def check_build_paths(label, data):
    """Fail on build-machine paths, except a module replace target in the Go build info.

    gomobile builds a generated module that replaces the bound module with its checkout
    directory, and the Go build info records that directory even under -trimpath.
    """
    if not PATH_PREFIXES:
        PATH_PREFIXES.extend(build_path_prefixes())
    replaced = sorted({m.group(1).decode() for m in MODINFO_REPLACE.finditer(data)})
    scrubbed = MODINFO_REPLACE.sub(b"\n=>\t\t", data)
    for prefix in PATH_PREFIXES:
        at = scrubbed.find(prefix)
        if at >= 0:
            context = scrubbed[max(0, at - 40):at + 120].decode("utf-8", "replace")
            fail(f"{label} embeds the build-machine path {prefix.decode()}: {context!r}")
    return replaced


def pe_machine(data, label):
    if data[:2] != b"MZ":
        fail(f"{label} is not a PE file")
    offset, = struct.unpack_from("<I", data, 0x3C)
    if data[offset:offset + 4] != b"PE\0\0":
        fail(f"{label} has no PE header")
    machine, = struct.unpack_from("<H", data, offset + 4)
    return PE_MACHINES.get(machine, hex(machine))


def binary_machine(path):
    data = path.read_bytes()
    if data[:4] == b"\x7fELF":
        return elf_info(data, path.name)
    return {"machine": pe_machine(data, path.name)}


def elf64_exports(data):
    shoff, = struct.unpack_from("<Q", data, 0x28)
    shentsize, shnum = struct.unpack_from("<HH", data, 0x3A)
    sections = [struct.unpack_from("<IIQQQQIIQQ", data, shoff + i * shentsize) for i in range(shnum)]
    names = set()
    for _, sh_type, _, _, offset, size, link, _, _, entsize in sections:
        if sh_type != 11:
            continue
        strtab = sections[link][4]
        for base in range(offset, offset + size, entsize):
            st_name, st_info, _, st_shndx = struct.unpack_from("<IBBH", data, base)
            if st_shndx != 0 and st_info >> 4 == 1:
                end = data.index(b"\0", strtab + st_name)
                names.add(data[strtab + st_name:end].decode())
    return names


def pe_exports(data):
    pe, = struct.unpack_from("<I", data, 0x3C)
    nsections, = struct.unpack_from("<H", data, pe + 6)
    optsize, = struct.unpack_from("<H", data, pe + 20)
    opt = pe + 24
    magic, = struct.unpack_from("<H", data, opt)
    export_rva, = struct.unpack_from("<I", data, opt + (112 if magic == 0x20B else 96))
    sections = [struct.unpack_from("<IIII", data, opt + optsize + i * 40 + 8) for i in range(nsections)]

    def offset(rva):
        for vsize, vaddr, rawsize, rawptr in sections:
            if vaddr <= rva < vaddr + max(vsize, rawsize):
                return rawptr + rva - vaddr
        fail(f"RVA {rva:#x} is outside every section")

    if export_rva == 0:
        return set()
    directory = offset(export_rva)
    count, names_rva = struct.unpack_from("<I", data, directory + 24)[0], struct.unpack_from("<I", data, directory + 32)[0]
    names = set()
    for i in range(count):
        start = offset(struct.unpack_from("<I", data, offset(names_rva) + 4 * i)[0])
        names.add(data[start:data.index(b"\0", start)].decode())
    return names


def exports(path, symbols):
    data = path.read_bytes()
    defined = elf64_exports(data) if data[:4] == b"\x7fELF" else pe_exports(data)
    missing = [s for s in symbols if s not in defined]
    if missing:
        fail(f"{path.name} does not export {', '.join(missing)}")
    return symbols


VERSION_REQUEST = json.dumps({"apiVersion": 3, "method": "xrayVersion"})


def parse_version_response(text, label):
    try:
        response = json.loads(text)
    except ValueError:
        fail(f"{label}: xrayVersion returned {text!r}")
    if not response.get("success") or not response.get("data", {}).get("version"):
        fail(f"{label}: xrayVersion failed: {text}")
    return response["data"]["version"]


def invoke_version(path):
    import ctypes

    lib = ctypes.CDLL(str(path.resolve()))
    lib.CGoInvoke.restype = ctypes.c_void_p
    lib.CGoInvoke.argtypes = [ctypes.c_char_p]
    lib.CGoFree.argtypes = [ctypes.c_void_p]
    pointer = lib.CGoInvoke(VERSION_REQUEST.encode())
    if not pointer:
        fail(f"{path.name}: CGoInvoke returned NULL")
    text = ctypes.string_at(pointer).decode("utf-8")
    lib.CGoFree(pointer)
    return parse_version_response(text, path.name)


def probe(cmd):
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except subprocess.TimeoutExpired:
        return {"exitCode": None, "output": "timed out"}
    output = (result.stdout + result.stderr).strip().splitlines()
    return {"exitCode": result.returncode, "output": output[0] if output else ""}


def cmd_desktop(args):
    root = Path(args.dir)
    exe, lib = root / args.exe, root / args.lib
    for path in (exe, lib, root / "libXray.h"):
        if not path.is_file() or path.stat().st_size == 0:
            fail(f"missing {path}")
    exe_info, lib_info = binary_machine(exe), binary_machine(lib)
    for path, info in ((exe, exe_info), (lib, lib_info)):
        if info["machine"] != args.machine:
            fail(f"{path.name} is {info['machine']}, expected {args.machine}")
        if info.get("stripped") is False:
            fail(f"{path.name} keeps {', '.join(info['debugSections'])}: it was built without -s -w")
        info["moduleReplacePaths"] = check_build_paths(path.name, path.read_bytes())
    exported = exports(lib, ["CGoInvoke", "CGoFree"])
    runtime_version = invoke_version(lib)
    print(f"{args.lib} xrayVersion -> {runtime_version}")

    help_run = subprocess.run([str(exe.resolve()), "-h"], capture_output=True, text=True, timeout=30)
    usage = help_run.stdout.strip()
    print(f"{args.exe} -h -> {help_run.returncode}: {usage}")
    if help_run.returncode != 0 or not usage.startswith(USAGE_PREFIX):
        fail(f"{args.exe} -h did not print the usage line (exit {help_run.returncode}): {usage or help_run.stderr.strip()}")
    bare = probe([str(exe.resolve())])
    if bare["exitCode"] in (0, None):
        fail(f"{args.exe} without arguments should refuse to run, got {bare}")
    probes = {}
    for name, extra in (("version", ["version"]), ("-version", ["-version"]), ("api statsquery", ["api", "statsquery"])):
        result = probe([str(exe.resolve()), *extra])
        result["supported"] = result["exitCode"] == 0
        probes[name] = result
        print(f"{args.exe} {name} -> {result}")
    write_json(args.out, {
        "binaries": {args.exe: exe_info, args.lib: lib_info},
        "libraryExports": exported,
        "runtimeXrayVersion": runtime_version,
        "cli": {"usage": usage, "noArguments": bare, "probes": probes},
    })


def cmd_aar(args):
    aar = Path(args.aar)
    with zipfile.ZipFile(aar) as archive:
        names = set(archive.namelist())
        if "classes.jar" not in names:
            fail("the AAR has no classes.jar")
        with zipfile.ZipFile(io.BytesIO(archive.read("classes.jar"))) as classes:
            class_count = sum(1 for n in classes.namelist() if n.endswith(".class"))
        if class_count == 0:
            fail("classes.jar holds no classes")
        libs = sorted(n for n in names if re.fullmatch(r"jni/[^/]+/[^/]+\.so", n))
        abis = {}
        for name in libs:
            abi = name.split("/")[1]
            data = archive.read(name)
            info = elf_info(data, name)
            if not info["stripped"]:
                fail(f"{name} keeps {', '.join(info['debugSections'])}: it was built without -s -w")
            info["moduleReplacePaths"] = check_build_paths(name, data)
            info["file"] = name.split("/")[2]
            info["size"] = archive.getinfo(name).file_size
            abis.setdefault(abi, []).append(info)
        missing = sorted(set(ANDROID_ABIS) - set(abis))
        if missing:
            fail(f"the AAR has no .so for {', '.join(missing)} (found {', '.join(sorted(abis)) or 'none'})")
        for abi, infos in abis.items():
            for info in infos:
                expected = ELF_MACHINES.get(ANDROID_ABIS.get(abi))
                if expected and info["machine"] != expected:
                    fail(f"jni/{abi}/{info['file']} is {info['machine']}, expected {expected}")
                if info["bits"] == 64 and info["loadAlign"] < PAGE_16K:
                    fail(f"jni/{abi}/{info['file']} is not 16 KB page aligned (LOAD align {info['loadAlign']})")
        manifest = archive.read("AndroidManifest.xml").decode("utf-8")
    min_sdk = re.search(r'minSdkVersion="(\d+)"', manifest)
    print(f"classes.jar: {class_count} classes; ABIs: {', '.join(sorted(abis))}")
    write_json(args.out, {
        "abis": sorted(abis),
        "minSdk": int(min_sdk.group(1)) if min_sdk else None,
        "classesJarClasses": class_count,
        "nativeLibraries": {abi: infos for abi, infos in sorted(abis.items())},
    })


def slice_name(lib):
    name = lib["SupportedPlatform"]
    if lib.get("SupportedPlatformVariant"):
        name += f"-{lib['SupportedPlatformVariant']}"
    return name


IOS_MEMORY_LIMIT = b"github.com/xtls/libxray/memory.forceFree"


def version_tuple(text):
    return tuple(int(p) for p in text.split("."))


def min_os_versions(binary):
    versions, command = set(), ""
    for line in run(["otool", "-arch", "all", "-l", str(binary)]).splitlines():
        line = line.strip()
        if line.startswith("cmd "):
            command = line.split()[1]
        elif command == "LC_BUILD_VERSION" and line.startswith("minos "):
            versions.add(line.split()[1])
        elif command.startswith("LC_VERSION_MIN_") and line.startswith("version "):
            versions.add(line.split()[1])
    return sorted(versions, key=version_tuple)


def cmd_xcframework(args):
    root = Path(args.path)
    plist = root / "Info.plist"
    if not plist.is_file():
        fail(f"{root} has no Info.plist")
    max_minos = dict(pair.split("=") for pair in args.max_minos.split(",")) if args.max_minos else {}
    libraries = plistlib.loads(plist.read_bytes()).get("AvailableLibraries", [])
    slices = []
    for lib in sorted(libraries, key=lambda l: l["LibraryIdentifier"]):
        path = root / lib["LibraryIdentifier"] / lib["LibraryPath"]
        if lib["LibraryPath"].endswith(".framework"):
            kind = "framework"
            binary = path / Path(lib["LibraryPath"]).stem
            if lib.get("BinaryPath"):
                binary = root / lib["LibraryIdentifier"] / lib["BinaryPath"]
            headers = sorted({p.name for p in path.glob("**/Headers/*.h")})
            modulemap = any(path.glob("**/Modules/module.modulemap"))
        else:
            kind = "library"
            binary = path
            header_dir = root / lib["LibraryIdentifier"] / lib.get("HeadersPath", "Headers")
            headers = sorted(p.name for p in header_dir.glob("*.h"))
            modulemap = (header_dir / "module.modulemap").is_file()
        ident = lib["LibraryIdentifier"]
        if not binary.exists() or binary.stat().st_size == 0:
            fail(f"{ident}: missing binary {binary}")
        data = binary.read_bytes()
        replaced = check_build_paths(ident, data)
        if not headers:
            fail(f"{ident}: no headers")
        archs = run(["lipo", "-archs", str(binary)]).split()
        if sorted(archs) != sorted(lib["SupportedArchitectures"]):
            fail(f"{ident}: binary has {archs}, Info.plist says {lib['SupportedArchitectures']}")
        if kind == "library":
            header_text = "".join(p.read_text(encoding="utf-8") for p in header_dir.glob("*.h"))
            missing = [s for s in ("CGoInvoke", "CGoFree") if s not in header_text]
            if missing or not modulemap:
                fail(f"{ident}: the headers must declare CGoInvoke and CGoFree beside a module.modulemap")
            symbols = run(["nm", "-gU", str(binary)])
            if not all(re.search(rf"\b_{s}$", symbols, re.M) for s in ("CGoInvoke", "CGoFree")):
                fail(f"{ident}: the archive does not define _CGoInvoke and _CGoFree")
        minos = min_os_versions(binary)
        platform, variant = lib["SupportedPlatform"], lib.get("SupportedPlatformVariant")
        limit = max_minos.get(platform) if variant in (None, "simulator") else None
        if limit and (not minos or version_tuple(minos[-1]) > version_tuple(limit)):
            fail(f"{ident}: minimum OS {minos or 'unknown'} is above the required {platform} {limit}")
        memory_limit = IOS_MEMORY_LIMIT in data
        if platform == "ios" and variant in (None, "simulator") and not memory_limit:
            fail(f"{ident}: built without the ios tag (libXray's iOS GC and memory limit are missing)")
        slices.append({
            "identifier": ident,
            "slice": slice_name(lib),
            "architectures": archs,
            "kind": kind,
            "binaryType": run(["file", "-b", str(binary)]).splitlines()[0],
            "minimumOS": minos,
            "iosMemoryLimit": memory_limit,
            "headers": headers,
            "moduleMap": modulemap,
            "moduleReplacePaths": replaced,
        })
        print(f"{ident}: {kind}, {' '.join(archs)}, min OS {', '.join(minos)}, iOS memory limit {memory_limit}, headers {', '.join(headers)}")
    found = {s["slice"] for s in slices}
    missing = sorted(set(args.require.split(",")) - found)
    if missing:
        fail(f"{root.name} lacks the {', '.join(missing)} slice(s); it has {', '.join(sorted(found))}")
    write_json(args.out, {"xcframework": root.name, "slices": slices})


SWIFTPM_LINKER_SETTINGS = ['.linkedFramework("CoreFoundation")', '.linkedFramework("Security")', '.linkedLibrary("resolv")']
SWIFTPM_PACKAGE = """// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "CoreSmoke",
    platforms: [.macOS("@PLATFORM@")],
    targets: [
        .binaryTarget(name: "LibXray", path: "@ZIP@"),
        .executableTarget(name: "smoke", dependencies: ["LibXray"], linkerSettings: [@LINKER@]),
    ]
)
"""
SWIFTPM_MAIN = """import Foundation
import LibXray

let request = strdup(#"@REQUEST@"#)
defer { free(request) }
guard let response = CGoInvoke(request) else { fatalError("CGoInvoke returned NULL") }
print(String(cString: response))
CGoFree(response)
"""


def cmd_swiftpm(args):
    """Consume the zip the way the app will: a SwiftPM binaryTarget, imported from Swift."""
    import shutil

    archive = Path(args.zip).resolve()
    work = Path(os.environ.get("RUNNER_TEMP", "/tmp")) / "swiftpm-smoke"
    shutil.rmtree(work, ignore_errors=True)
    (work / "Sources" / "smoke").mkdir(parents=True)
    shutil.copy(archive, work / archive.name)
    (work / "Package.swift").write_text(
        SWIFTPM_PACKAGE.replace("@PLATFORM@", args.platform).replace("@ZIP@", archive.name)
        .replace("@LINKER@", ", ".join(SWIFTPM_LINKER_SETTINGS)), encoding="utf-8")
    (work / "Sources" / "smoke" / "main.swift").write_text(
        SWIFTPM_MAIN.replace("@REQUEST@", VERSION_REQUEST), encoding="utf-8")

    checksum = run(["swift", "package", "compute-checksum", archive.name], cwd=work)
    if checksum != sha256(archive):
        fail(f"swift package compute-checksum gave {checksum}, not the zip's sha256")
    build = subprocess.run(["swift", "build", "-c", "release"], cwd=work, capture_output=True, text=True)
    output = build.stdout + build.stderr
    print(output)
    if build.returncode != 0:
        fail("the SwiftPM smoke package does not build against the xcframework")
    newer = [l.strip() for l in output.splitlines() if "built for newer" in l]
    if newer:
        fail(f"the xcframework targets a newer macOS than {args.platform}: {newer[0]}")
    binary = Path(run(["swift", "build", "-c", "release", "--show-bin-path"], cwd=work)) / "smoke"
    result = subprocess.run([str(binary)], capture_output=True, text=True, timeout=60)
    if result.returncode != 0:
        fail(f"the SwiftPM smoke binary exited {result.returncode}: {result.stderr.strip()}")
    version = parse_version_response(result.stdout.strip(), "SwiftPM smoke")
    print(f"SwiftPM: checksum {checksum}; macOS {args.platform} executable calls CGoInvoke -> Xray {version}")
    write_json(args.out, {
        "checksum": checksum,
        "deploymentTarget": f"macOS {args.platform}",
        "linkerSettings": SWIFTPM_LINKER_SETTINGS,
        "runtimeXrayVersion": version,
    })


def core_version(module_dir):
    source = (Path(module_dir) / "core" / "core.go").read_text(encoding="utf-8")
    parts = [re.search(rf"Version_{c}\s+byte\s*=\s*(\d+)", source) for c in "xyz"]
    if not all(parts):
        fail("cannot read Xray-core's version from core/core.go")
    return ".".join(p.group(1) for p in parts)


def cmd_info(args):
    src = Path(args.src)
    goversion = run(["go", "env", "GOVERSION"], cwd=src)
    if goversion != f"go{os.environ['GO_VERSION']}":
        fail(f"built with {goversion}, but the pin is go{os.environ['GO_VERSION']}")
    module_version, module_dir = run(
        ["go", "list", "-m", "-f", "{{.Version}}|{{.Dir}}", XRAY_CORE_MODULE], cwd=src
    ).split("|", 1)
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
        "asset": f"libxray-{args.target}.zip",
        "runner": {
            "os": os.environ.get("ImageOS"),
            "image": os.environ.get("ImageVersion"),
            "arch": os.environ.get("RUNNER_ARCH"),
        },
        "libxrayCommit": run(["git", "rev-parse", "HEAD"], cwd=src),
        "xrayCore": {"moduleVersion": module_version, "version": core_version(module_dir)},
        "toolchain": tools,
        "buildPatch": json.loads(Path(args.patch).read_text(encoding="utf-8")) if args.patch else None,
        "contents": contents,
    })


def resolve_xray_core(module_version, version):
    match = re.search(r"-([0-9a-f]{12})$", module_version)
    short = match.group(1) if match else None
    tag = f"v{version}"
    commit = ls_remote_tag(XRAY_CORE_URL, tag)
    if commit and short and commit.startswith(short):
        return {"tag": tag, "commit": commit}
    print(f"::warning::Xray-core {module_version} is not the {tag} release tag ({commit})")
    return {"tag": None, "commit": short}


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def zip_listing(path):
    with zipfile.ZipFile(path) as archive:
        parts = [n.rstrip("/").split("/") for n in archive.namelist()]
    depth = 1 if {p[0] for p in parts} == {path.stem} else 0
    return sorted({p[depth] for p in parts if len(p) > depth})


def cmd_manifest(args):
    dist = Path(args.dist)
    env = os.environ
    infos = {}
    for path in sorted(dist.glob("libxray-*.json")):
        info = json.loads(path.read_text(encoding="utf-8"))
        infos[info["target"]] = info
    missing = sorted(set(TARGETS) - set(infos))
    if missing:
        fail(f"no build for {', '.join(missing)}")

    commit = env["LIBXRAY_COMMIT"]
    cores = {(i["xrayCore"]["moduleVersion"], i["xrayCore"]["version"]) for i in infos.values()}
    for target, info in infos.items():
        if info["libxrayCommit"] != commit:
            fail(f"{target} was built from {info['libxrayCommit']}, not {commit}")
    if len(cores) != 1:
        fail(f"the builds disagree on the Xray-core version: {sorted(cores)}")
    module_version, version = cores.pop()

    assets = []
    for target in TARGETS:
        info = infos[target]
        path = dist / info["asset"]
        if not path.is_file():
            fail(f"missing {path}")
        digest = sha256(path)
        contents = info["contents"]
        swiftpm = contents.get("swiftpm")
        if swiftpm and swiftpm["checksum"] != digest:
            fail(f"{target}: the SwiftPM checksum {swiftpm['checksum']} is not the zip's sha256 {digest}")
        for runtime in (contents.get("runtimeXrayVersion"), (swiftpm or {}).get("runtimeXrayVersion")):
            if runtime and runtime != version:
                fail(f"{target} reports Xray {runtime} at runtime, but its module is {version}")
        assets.append({
            "name": info["asset"],
            "target": target,
            "size": path.stat().st_size,
            "sha256": digest,
            **({"swiftpmChecksum": swiftpm["checksum"]} if swiftpm else {}),
            "files": zip_listing(path),
            "runner": info["runner"],
            "toolchain": info["toolchain"],
            "buildPatch": info["buildPatch"],
            "contents": info["contents"],
        })

    manifest = {
        "schema": 1,
        "release": f"core-{env['LIBXRAY_TAG']}",
        "builtAt": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "build": {
            "repository": env["GITHUB_REPOSITORY"],
            "workflowCommit": env["GITHUB_SHA"],
            "ref": env["GITHUB_REF"],
            "run": f"{env['GITHUB_SERVER_URL']}/{env['GITHUB_REPOSITORY']}/actions/runs/{env['GITHUB_RUN_ID']}",
        },
        "libxray": {
            "repository": "XTLS/libXray",
            "tag": env["LIBXRAY_TAG"],
            "commit": commit,
            "license": "MIT",
        },
        "xrayCore": {
            "repository": "XTLS/Xray-core",
            "module": XRAY_CORE_MODULE,
            "version": version,
            "moduleVersion": module_version,
            **resolve_xray_core(module_version, version),
            "license": "MPL-2.0",
        },
        "toolchain": {
            "go": env["GO_VERSION"],
            "gomobile": env["LIBXRAY_GOMOBILE_VERSION"],
            "ndk": env["NDK_VERSION"],
            "ndkRevision": env["NDK_REVISION"],
            "androidPlatform": env["ANDROID_PLATFORM"],
            "java": f"temurin {env['JAVA_VERSION']}",
            "xcode": env["XCODE_VERSION"],
            "python": env["PYTHON_VERSION"],
        },
        "notes": MANIFEST_NOTES,
        "assets": assets,
    }
    write_json(dist / "manifest.json", manifest)
    sums = [(a["sha256"], a["name"]) for a in assets] + [(sha256(dist / "manifest.json"), "manifest.json")]
    (dist / "SHA256SUMS").write_text("".join(f"{h}  {n}\n" for h, n in sorted(sums, key=lambda s: s[1])), encoding="utf-8")
    write_notes(dist / "NOTES.md", manifest)


def describe(asset):
    contents = asset["contents"]
    if "abis" in contents:
        return f"`libXray.aar` ({', '.join(contents['abis'])}; minSdk {contents['minSdk']}) + sources jar"
    if "slices" in contents:
        kinds = sorted({s["kind"] for s in contents["slices"]})
        text = f"`{contents['xcframework']}` ({'/'.join(kinds)}): " + ", ".join(s["identifier"] for s in contents["slices"])
        if asset.get("swiftpmChecksum"):
            text += f". SwiftPM `binaryTarget` checksum `{asset['swiftpmChecksum']}`"
        return text
    names = list(contents["binaries"])
    return f"`{names[1]}` + `libXray.h` + desktop `{names[0]}` (`xray run` only)"


def write_notes(path, manifest):
    lib, core = manifest["libxray"], manifest["xrayCore"]
    rows = "\n".join(f"| `{a['name']}` | {describe(a)} |" for a in manifest["assets"])
    tool = manifest["toolchain"]
    path.write_text(
        f"Xray core for the BodoVPN apps, built from [XTLS/libXray {lib['tag']}]"
        f"({LIBXRAY_URL}/tree/{lib['commit']}) (MIT) with Xray-core {core['version']}.\n\n"
        "This is a build input for the apps, not an app release. The app downloads are on the "
        f"[latest release](https://github.com/{manifest['build']['repository']}/releases/latest).\n\n"
        f"| Asset | Contents |\n| --- | --- |\n{rows}\n\n"
        f"Toolchain: Go {tool['go']}, gomobile {tool['gomobile']}, NDK {tool['ndk']} ({tool['ndkRevision']}), "
        f"{tool['androidPlatform']}, Java {tool['java']}, Xcode {tool['xcode']}.\n"
        "Every pinned version and each asset's details are in `manifest.json`; checksums are in `SHA256SUMS`. "
        f"Verify a download with `gh attestation verify <file> -R {manifest['build']['repository']}`.\n",
        encoding="utf-8",
    )


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("resolve").set_defaults(func=cmd_resolve)

    p = sub.add_parser("zip")
    p.add_argument("src")
    p.add_argument("out")
    p.add_argument("--flat", action="store_true", help="put src's contents at the zip root (SwiftPM binaryTarget layout)")
    p.set_defaults(func=cmd_zip)

    p = sub.add_parser("desktop")
    p.add_argument("dir")
    p.add_argument("--exe", required=True)
    p.add_argument("--lib", required=True)
    p.add_argument("--machine", required=True)
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_desktop)

    p = sub.add_parser("aar")
    p.add_argument("aar")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_aar)

    p = sub.add_parser("xcframework")
    p.add_argument("path")
    p.add_argument("--require", required=True)
    p.add_argument("--max-minos", default="", help="e.g. ios=15.0,macos=13.0")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_xcframework)

    p = sub.add_parser("swiftpm")
    p.add_argument("zip")
    p.add_argument("--platform", required=True, help="the smoke package's macOS deployment target, e.g. 13")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_swiftpm)

    p = sub.add_parser("info")
    p.add_argument("--src", required=True)
    p.add_argument("--target", required=True, choices=TARGETS)
    p.add_argument("--contents", required=True)
    p.add_argument("--tool", action="append")
    p.add_argument("--merge", action="append", help="key=file: add a JSON file to contents under key")
    p.add_argument("--patch")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_info)

    p = sub.add_parser("patch-gomobile")
    p.add_argument("--src", required=True)
    p.add_argument("--script", required=True, choices=["android.py", "apple_gomobile.py"])
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_patch_gomobile)

    p = sub.add_parser("manifest")
    p.add_argument("dist")
    p.set_defaults(func=cmd_manifest)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
