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
TARGETS = ["android", "apple-cgo", "apple-gomobile", "linux-x64", "windows-x64"]


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

    with zipfile.ZipFile(args.out, "w") as archive:
        archive.writestr(entry(f"{src.name}/", stat.S_IFDIR | 0o755), b"")
        for path in zip_entries(src):
            name = f"{src.name}/{path.relative_to(src).as_posix()}"
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
    return {
        "machine": ELF_MACHINES.get(machine, hex(machine)),
        "bits": 64 if is64 else 32,
        "loadAlign": min(load_aligns) if load_aligns else 0,
        "programInterpreter": interp,
    }


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
    for label, info in ((args.exe, exe_info), (args.lib, lib_info)):
        if info["machine"] != args.machine:
            fail(f"{label} is {info['machine']}, expected {args.machine}")
    exported = exports(lib, ["CGoInvoke", "CGoFree"])

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
            info = elf_info(archive.read(name), name)
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


def cmd_xcframework(args):
    root = Path(args.path)
    plist = root / "Info.plist"
    if not plist.is_file():
        fail(f"{root} has no Info.plist")
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
        if not binary.exists() or binary.stat().st_size == 0:
            fail(f"{lib['LibraryIdentifier']}: missing binary {binary}")
        if not headers:
            fail(f"{lib['LibraryIdentifier']}: no headers")
        archs = run(["lipo", "-archs", str(binary)]).split()
        if sorted(archs) != sorted(lib["SupportedArchitectures"]):
            fail(f"{lib['LibraryIdentifier']}: binary has {archs}, Info.plist says {lib['SupportedArchitectures']}")
        slices.append({
            "identifier": lib["LibraryIdentifier"],
            "slice": slice_name(lib),
            "architectures": archs,
            "kind": kind,
            "binaryType": run(["file", "-b", str(binary)]).splitlines()[0],
            "headers": headers,
            "moduleMap": modulemap,
        })
        print(f"{lib['LibraryIdentifier']}: {kind}, {' '.join(archs)}, headers {', '.join(headers)}")
    found = {s["slice"] for s in slices}
    missing = sorted(set(args.require.split(",")) - found)
    if missing:
        fail(f"{root.name} lacks the {', '.join(missing)} slice(s); it has {', '.join(sorted(found))}")
    write_json(args.out, {"xcframework": root.name, "slices": slices})


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
        "contents": json.loads(Path(args.contents).read_text(encoding="utf-8")),
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
        names = {"/".join(n.rstrip("/").split("/")[1:2]) for n in archive.namelist()}
    return sorted(n for n in names if n)


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
        assets.append({
            "name": info["asset"],
            "target": target,
            "size": path.stat().st_size,
            "sha256": sha256(path),
            "files": zip_listing(path),
            "runner": info["runner"],
            "toolchain": info["toolchain"],
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
        return f"`{contents['xcframework']}` ({'/'.join(kinds)}): " + ", ".join(s["identifier"] for s in contents["slices"])
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
        "Every pinned version and each asset's details are in `manifest.json`; checksums are in `SHA256SUMS`.\n",
        encoding="utf-8",
    )


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("resolve").set_defaults(func=cmd_resolve)

    p = sub.add_parser("zip")
    p.add_argument("src")
    p.add_argument("out")
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
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_xcframework)

    p = sub.add_parser("info")
    p.add_argument("--src", required=True)
    p.add_argument("--target", required=True, choices=TARGETS)
    p.add_argument("--contents", required=True)
    p.add_argument("--tool", action="append")
    p.add_argument("--out", required=True)
    p.set_defaults(func=cmd_info)

    p = sub.add_parser("manifest")
    p.add_argument("dist")
    p.set_defaults(func=cmd_manifest)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
