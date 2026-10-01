#!/usr/bin/env python3
"""Build and publish this fork to one Docker CPA instance over OpenSSH.

Python standard library locally; Python 3 and PyYAML on the target. The same
file runs as a detached remote worker so an SSH disconnect cannot interrupt a
stop/backup/switch/start transaction. No Git commits, tags or image pulls.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import datetime as dt
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid


PLUGIN = "cpa-key-policy"
VERSION_FILES = ("internal/plugin/types.go", "web/package.json", "web/package-lock.json")
EMBED = "internal/plugin/web/dist/index.html"
TERMINAL = {"succeeded", "failed", "restored", "needs_attention"}
VERSION_RE = re.compile(r"(\bVersion\s*=\s*\")([0-9]+\.[0-9]+\.[0-9]+)(\")")
PROGRAM_SOURCE = Path(__file__).read_bytes() if "__file__" in globals() else b""


class PublishError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise PublishError(message)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read_json(path):
    return json.loads(Path(path).read_bytes())


def atomic_write(path, raw, mode=0o600):
    path = Path(path)
    descriptor, name = tempfile.mkstemp(prefix="." + path.name + ".", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def write_json(path, value):
    atomic_write(path, (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode())


@contextlib.contextmanager
def locked(path):
    with open(path, "a+b") as stream:
        try:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise PublishError("已有发布进程持有锁：" + str(path)) from None
        yield


def command(args, *, cwd=None, timeout=60, env=None, show=False, data=None):
    process = subprocess.Popen(
        [str(arg) for arg in args], cwd=cwd, env=env,
        stdin=subprocess.DEVNULL if data is None else subprocess.PIPE,
        stdout=None if show else subprocess.PIPE,
        stderr=None if show else subprocess.PIPE, start_new_session=True,
    )
    try:
        output, error_output = process.communicate(data, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise PublishError(f"{Path(args[0]).name} 执行超时；不能据此认定远端操作未执行") from None
    finally:
        if process.poll() is None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.wait()
    if process.returncode:
        # Docker inspect/config output may contain credentials. Keep it private.
        detail = ""
        if Path(args[0]).name == "migration-check" and error_output:
            detail = "：" + error_output.decode(errors="replace").strip()[-2000:]
        raise PublishError(f"{Path(args[0]).name} 执行失败，退出码 {process.returncode}" + detail)
    return output or b""


def version_tuple(value):
    require(bool(re.fullmatch(r"\d+\.\d+\.\d+", value)), "不支持的版本格式：" + value)
    return tuple(map(int, value.split(".")))


def next_version(*versions):
    major, minor, _ = max(version_tuple(value) for value in versions)
    return f"{major}.{minor + 1}.0"


def source_version(root):
    match = VERSION_RE.search((root / VERSION_FILES[0]).read_text())
    require(match is not None, "找不到插件 Version 常量")
    value = match.group(2)
    package = read_json(root / VERSION_FILES[1])
    lock = read_json(root / VERSION_FILES[2])
    require(package["version"] == lock["version"] == lock["packages"][""]["version"] == value,
            "插件、package.json、package-lock.json 版本不一致，请先解决本地冲突")
    return value


def bump_source(root, old, new):
    path = root / VERSION_FILES[0]
    raw = path.read_text()
    updated, count = VERSION_RE.subn(lambda m: m.group(1) + new + m.group(3), raw)
    require(count == 1 and source_version(root) == old, "版本文件在准备过程中发生变化")
    path.write_text(updated)
    # Replace only the root package versions, preserving the dependency graph.
    for name, expected in [(VERSION_FILES[1], 1), (VERSION_FILES[2], 2)]:
        path = root / name
        raw = path.read_text()
        pattern = re.compile(r'("version"\s*:\s*")' + re.escape(old) + r'(")')
        matches = list(pattern.finditer(raw))
        require(len(matches) >= expected, "找不到根包版本：" + name)
        for match in reversed(matches[:expected]):
            raw = raw[:match.start()] + match.group(1) + new + match.group(2) + raw[match.end():]
        path.write_text(raw)
    require(source_version(root) == new, "递增后的版本不一致")


def ssh_arguments(value):
    args = shlex.split(value)
    require(args and Path(args[0]).name == "ssh", "--ssh 应为完整 ssh 命令或 ssh 主机别名")
    # OpenSSH options with operands; reject a remote shell command after target.
    operand_options = set("BbcFijJlmoSp")
    position = 1
    while position < len(args):
        item = args[position]
        if item == "--":
            position += 1
            break
        if not item.startswith("-"):
            break
        require(len(item) > 1 and item[1] in operand_options | set("46ACqv"),
                "--ssh 只支持连接参数，不能包含后台、转发、TTY 或控制命令")
        if len(item) == 2 and item[1] in operand_options:
            position += 1
            require(position < len(args), "SSH 参数缺少值")
        position += 1
    require(position == len(args) - 1 and not args[position].startswith("-"),
            "--ssh 只能指定连接参数和主机，不能包含远端命令")
    return [args[0], "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
            "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15",
            "-o", "ServerAliveCountMax=3", *args[1:]]


def ssh_call(ssh, action, request, *, payload=None, timeout=90):
    bootstrap = "import base64;exec(compile(base64.b64decode(" + repr(base64.b64encode(PROGRAM_SOURCE).decode()) + "),'publish.py','exec'))"
    encoded = base64.b64encode(json.dumps(request).encode()).decode()
    remote = shlex.join(["python3", "-c", bootstrap, "--_remote", action, encoded])
    raw = command([*ssh, remote], data=payload, timeout=timeout)
    try:
        result = json.loads(raw)
    except ValueError:
        raise PublishError("SSH 未返回有效发布协议响应，请检查登录脚本是否向 stdout 输出内容") from None
    require(result.get("ok"), result.get("error", "远端操作失败"))
    return result["result"]


def yaml_document(raw):
    try:
        import yaml
    except ImportError:
        raise PublishError("远端需要已有 Python 3 和 PyYAML；脚本不会自动安装系统软件") from None
    return yaml, yaml.safe_load(raw)


def patch_config(raw, version):
    yaml, before = yaml_document(raw)
    tree = yaml.compose(raw.decode())

    def node_at(path):
        node = tree
        for name in path:
            require(isinstance(node, yaml.MappingNode), "插件配置必须使用 YAML 映射")
            matches = [value for key, value in node.value if key.value == name]
            require(len(matches) == 1, "插件配置字段缺失或重复：" + ".".join(path))
            node = matches[0]
        return node

    prefix = ["plugins", "configs", PLUGIN, "store"]
    version_node = node_at(prefix + ["version"])
    require(isinstance(version_node, yaml.ScalarNode), "store.version 必须为标量")
    edits = [(version_node.start_mark.index, version_node.end_mark.index, json.dumps(version))]
    store = before["plugins"]["configs"][PLUGIN]["store"]
    if "release-tag" in store:
        tag = node_at(prefix + ["release-tag"])
        require(isinstance(tag, yaml.ScalarNode), "store.release-tag 必须为标量")
        edits.append((tag.start_mark.index, tag.end_mark.index, '""'))
    text = raw.decode()
    for start, end, value in sorted(edits, reverse=True):
        text = text[:start] + value + text[end:]
    expected = json.loads(json.dumps(before))
    expected_store = expected["plugins"]["configs"][PLUGIN]["store"]
    expected_store["version"] = version
    if "release-tag" in expected_store:
        expected_store["release-tag"] = ""
    require(yaml.safe_load(text) == expected, "YAML 修改影响了版本以外的配置，拒绝发布")
    return text.encode()


def docker_inspect(name):
    return json.loads(command(["docker", "inspect", name]))[0]


def container_path(container, value):
    path = PurePosixPath(value)
    if not path.is_absolute():
        path = PurePosixPath(container["Config"].get("WorkingDir") or "/") / path
    return str(path)


def mounted_file(container, path):
    path = PurePosixPath(container_path(container, path))
    for mount in sorted(container["Mounts"], key=lambda m: len(m["Destination"]), reverse=True):
        destination = PurePosixPath(mount["Destination"])
        if path == destination or destination in path.parents:
            require(mount.get("RW"), "插件文件或数据目录是只读挂载")
            return str(Path(mount["Source"]).joinpath(*path.relative_to(destination).parts).resolve())
    raise PublishError("容器路径未持久化挂载：" + str(path))


def find_container(root, name):
    if name:
        candidates = [docker_inspect(name)]
    else:
        ids = command(["docker", "ps", "-aq"]).decode().split()
        candidates = json.loads(command(["docker", "inspect", *ids])) if ids else []
    found = []
    for item in candidates:
        for mount in item.get("Mounts", []):
            path = Path(mount["Source"]).resolve()
            if path.name == "config.yaml" and path.is_relative_to(root) and path.is_file():
                _, cfg = yaml_document(path.read_bytes())
                if isinstance(cfg, dict) and PLUGIN in (cfg.get("plugins", {}).get("configs") or {}):
                    found.append((item, path, cfg))
    require(len(found) == 1, "部署目录内未找到唯一 CPA 容器；可用 --container 指定")
    return found[0]


def management_key(root, container, config):
    path = root / ".env"
    if path.is_file():
        for line in path.read_text().splitlines():
            name, separator, value = line.strip().partition("=")
            if separator and name in ("MANAGEMENT_KEY", "CPA_MANAGEMENT_KEY"):
                values = shlex.split(value, comments=True)
                if len(values) == 1 and values[0]:
                    return values[0]
    variables = dict(item.split("=", 1) for item in container["Config"].get("Env", []) if "=" in item)
    for name in ("MANAGEMENT_PASSWORD", "CPA_MANAGEMENT_KEY"):
        if variables.get(name):
            return variables[name]
    value = (config.get("remote-management") or {}).get("secret-key", "")
    if value and not value.startswith("$2"):
        return value
    raise PublishError("无法取得管理密钥：需要目标 .env 的 MANAGEMENT_KEY 或 CPA 容器的 MANAGEMENT_PASSWORD")


def management_url(container, config):
    require(not (config.get("tls") or {}).get("enable"), "当前脚本要求 CPA 容器内使用 HTTP，可由外部反代提供 HTTPS")
    port = str(config.get("port", 8317))
    published = (container["NetworkSettings"].get("Ports") or {}).get(port + "/tcp") or []
    for mapping in published:
        host = mapping["HostIp"]
        if host in ("", "0.0.0.0", "::"):
            host = "127.0.0.1"
        if ":" in host:
            host = "[" + host + "]"
        return f"http://{host}:{mapping['HostPort']}"
    for network in container["NetworkSettings"].get("Networks", {}).values():
        if network.get("IPAddress"):
            return f"http://{network['IPAddress']}:{port}"
    raise PublishError("无法确定 CPA 管理地址")


def http_get(url, key="", *, binary=False):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    headers = {"Authorization": "Bearer " + key} if key else {}
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=8) as response:
            raw = response.read()
            return raw if binary else json.loads(raw)
    except (OSError, ValueError, urllib.error.HTTPError):
        raise PublishError("CPA 健康或管理接口尚未就绪") from None


def runtime_plugin(url, key):
    data = http_get(url + "/v0/management/plugins", key)
    items = [item for item in data.get("plugins", []) if item.get("id") == PLUGIN]
    require(len(items) == 1, "管理接口未返回目标插件")
    item = items[0]
    require(item.get("registered") and item.get("effective_enabled"), "插件未注册或未实际启用")
    return item


def paired_data(state_path, usage_path):
    state, usage = read_json(state_path), read_json(usage_path)
    require(state.get("dataset_id") and state.get("dataset_id") == usage.get("dataset_id"),
            "state/usage 数据集不匹配")
    require(state.get("version") in (3, 4, 5, 6) and usage.get("version") in (3, 4, 5, 6),
            "目标数据格式不受当前发布脚本支持")
    return state, usage


def inspect_target(request):
    require(sys.version_info >= (3, 9), "远端需要 Python 3.9 或更新版本")
    require(platform.system() == "Linux" and platform.machine() in ("x86_64", "amd64"),
            "目标必须为 Linux x64")
    root = Path(request["remote_dir"]).resolve(strict=True)
    require(root.is_dir() and len(root.parts) >= 3, "请指定实例专属部署目录")
    container, config_path, config = find_container(root, request.get("container"))
    require(container["State"]["Running"], "CPA 容器未运行，请先恢复现有实例")
    require(config["plugins"].get("enabled"), "CPA 插件功能未启用")
    item = config["plugins"]["configs"][PLUGIN]
    require(item.get("enabled"), "目标插件未启用")
    configured_version = item.get("store", {}).get("version", "")
    version_tuple(configured_version)
    patch_config(config_path.read_bytes(), configured_version)
    image = json.loads(command(["docker", "image", "inspect", container["Image"]]))[0]
    require(image.get("Architecture") == "amd64", "CPA 容器镜像必须为 amd64")
    libc = command(["docker", "exec", container["Id"], "ldd", "--version"]).decode()
    match = re.search(r"(?:GLIBC|GLIBC[^\n]*|ldd[^\n]*)\s(\d+)\.(\d+)\s*(?:\n|$)", libc.splitlines()[0] + "\n")
    require(match is not None and tuple(map(int, match.groups())) >= (2, 36), "CPA 容器需要 glibc 2.36 或更新版本")
    url = management_url(container, config)
    key = management_key(root, container, config)
    plugin = runtime_plugin(url, key)
    version = plugin.get("metadata", {}).get("version", "")
    require(version == configured_version, "配置版本与正在运行的插件版本不同")
    plugin_path = mounted_file(container, plugin["path"])
    plugin_dir = mounted_file(container, config["plugins"].get("dir", "plugins"))
    state_path = mounted_file(container, item.get("state_file", "cpa-key-policy-state.json"))
    usage_path = str(Path(state_path).with_name(PLUGIN + "-usage.json"))
    state, usage = paired_data(state_path, usage_path)
    http_get(url + "/healthz")
    audit = Path(state_path).with_name(PLUGIN + "-audit.jsonl")
    files = [str(config_path), plugin_path, state_path, usage_path]
    files.extend(str(path) for path in [audit, *audit.parent.glob(audit.name + ".*")] if path.is_file())
    for suffix in (".before-v5.json", ".before-v6.json"):
        recovery = Path(state_path + suffix)
        files.extend(str(path) for path in [recovery, *recovery.parent.glob(recovery.name + ".*")] if path.is_file())
    needed = sum(Path(path).stat().st_size for path in files) * 3 + 64 * 1024 * 1024
    require(shutil.disk_usage(root).free >= needed, "远端可用空间不足以保存发布包及完整备份")
    return {
        "remote_dir": str(root), "container": container["Name"].lstrip("/"),
        "container_id": container["Id"], "image_id": container["Image"],
        "config_path": str(config_path), "config_sha256": sha(config_path.read_bytes()),
        "plugin_path": plugin_path, "plugin_container_path": plugin["path"],
        "plugin_dir": plugin_dir,
        "plugin_dir_container": container_path(container, config["plugins"].get("dir", "plugins")),
        "plugin_sha256": sha(Path(plugin_path).read_bytes()),
        "state_path": state_path, "usage_path": usage_path, "backup_files": sorted(set(files)),
        "version": version, "timezone": item.get("usage_timezone") or "Asia/Shanghai",
        "dataset_id": state["dataset_id"], "state_version": state["version"],
        "usage_version": usage["version"], "keys": len(state.get("keys", [])),
        "models": len(state.get("models", [])), "management_url": url,
    }


def remote_directory(request):
    release_id = request["release_id"]
    require(bool(re.fullmatch(r"[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}", release_id)), "无效发布编号")
    root = Path(request["remote_dir"]).resolve(strict=True)
    require(len(root.parts) >= 3, "无效部署目录")
    return root / ".cpa-key-policy-publish" / release_id


def remote_stage(request):
    directory = remote_directory(request)
    expected = request["files"]
    libraries = [name for name in expected if re.fullmatch(r"cpa-key-policy-v\d+\.\d+\.\d+\.so", name)]
    require(len(libraries) == 1 and set(expected) == {libraries[0], "publish.py", "request.json", "migration-check"},
            "发布包文件名不符合协议")
    directory.parent.mkdir(mode=0o700, exist_ok=True)
    if directory.exists():
        require((directory / "upload.json").is_file() and read_json(directory / "upload.json") == request,
                "远端目录不属于本次上传，拒绝覆盖")
        require(not (directory / "status.json").exists(), "发布包已上传，不能重复覆盖")
    else:
        directory.mkdir(mode=0o700)
        write_json(directory / "upload.json", request)
    received = set()
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            require(member.isfile() and member.name in expected and member.name not in received,
                    "发布包包含非预期文件")
            require(member.size <= 128 * 1024 * 1024, "发布文件过大")
            raw = archive.extractfile(member).read()
            require(sha(raw) == expected[member.name], "上传文件校验失败：" + member.name)
            atomic_write(directory / member.name, raw, 0o700 if member.name == "migration-check" else 0o600)
            received.add(member.name)
    require(received == set(expected), "发布包文件不完整")
    write_json(directory / "status.json", {"phase": "staged", "updated_at": now()})
    return {"directory": str(directory), "phase": "staged"}


def remote_launch(request):
    directory = remote_directory(request)
    status = read_json(directory / "status.json")
    if status["phase"] != "staged":
        return status
    with locked(directory / "launch.lock"):
        status = read_json(directory / "status.json")
        if status["phase"] != "staged":
            return status
        if (directory / "worker.json").exists():
            return remote_status(request)
        with open(directory / "worker.log", "ab") as log:
            worker = subprocess.Popen(
                [sys.executable, str(directory / "publish.py"), "--_worker", str(directory)],
                stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True,
            )
        # The worker owns phase updates; launch only records its process identity.
        write_json(directory / "worker.json", {"pid": worker.pid})
    return {"phase": "launched", "pid": worker.pid}


def remote_status(request):
    directory = remote_directory(request)
    if not directory.exists():
        return {"phase": "missing"}
    if not (directory / "status.json").exists():
        require((directory / "upload.json").is_file(), "远端目录没有发布状态，请检查现场")
        return {"phase": "uploading"}
    status = read_json(directory / "status.json")
    if status["phase"] not in TERMINAL and (directory / "worker.json").exists():
        pid = read_json(directory / "worker.json")["pid"]
        try:
            args = Path(f"/proc/{pid}/cmdline").read_bytes()
            alive = str(directory).encode() in args
        except FileNotFoundError:
            alive = False
        if not alive:
            status = {**status, "phase": "needs_attention", "error": "远端进程已结束但未记录终态；保留现场，禁止重复切换或自动恢复旧账本"}
    return status


def backup_files(directory, target):
    root = Path(target["remote_dir"]) / "backups"
    root.mkdir(mode=0o700, exist_ok=True)
    backup = root / ("plugin-" + directory.name)
    backup.mkdir(mode=0o700)
    entries = []
    for index, value in enumerate(target["backup_files"]):
        path = Path(value)
        info = path.stat()
        raw = path.read_bytes()
        name = f"{index:02d}-{path.name}"
        atomic_write(backup / name, raw)
        entries.append({"source": str(path), "file": name, "sha256": sha(raw),
                        "mode": info.st_mode & 0o777, "uid": info.st_uid, "gid": info.st_gid})
    write_json(backup / "manifest.json", {"created_at": now(), "target": target, "files": entries})
    return backup


def restore_before_start(backup, target):
    # Only used before docker start for the new version has even been attempted.
    manifest = read_json(backup / "manifest.json")
    for entry in manifest["files"]:
        if entry["source"] not in (target["config_path"], target["plugin_path"]):
            continue
        raw = (backup / entry["file"]).read_bytes()
        require(sha(raw) == entry["sha256"], "恢复文件校验失败")
        atomic_write(entry["source"], raw, entry["mode"])
        os.chown(entry["source"], entry["uid"], entry["gid"])
    command(["docker", "start", target["container_id"]], timeout=60)


def verify_runtime(target, version, plugin_container_path, timeout=90):
    deadline = time.monotonic() + timeout
    last_error = "尚未就绪"
    while time.monotonic() < deadline:
        try:
            container = docker_inspect(target["container_id"])
            require(container["State"]["Running"], "CPA 容器未运行")
            require(container["Image"] == target["image_id"], "CPA 镜像发生变化")
            _, config = yaml_document(Path(target["config_path"]).read_bytes())
            key = management_key(Path(target["remote_dir"]), container, config)
            url = management_url(container, config)
            http_get(url + "/healthz")
            item = runtime_plugin(url, key)
            require(item.get("metadata", {}).get("version") == version, "运行版本不符")
            require(item.get("path") == plugin_container_path, "实际加载路径不符")
            keys = http_get(url + f"/v0/management/plugins/{PLUGIN}/keys", key)
            models = http_get(url + f"/v0/management/plugins/{PLUGIN}/models", key)
            require(len(keys.get("keys", [])) == target["keys"], "Key 数量发生变化")
            require(len(models.get("models", [])) == target["models"], "模型数量发生变化")
            page = http_get(url + f"/v0/resource/plugins/{PLUGIN}/index.html", binary=True)
            return {"healthy": True, "version": version, "keys": target["keys"],
                    "models": target["models"], "ui_sha256": sha(page)}
        except PublishError as error:
            last_error = str(error)
            time.sleep(2)
    raise PublishError("启动验收失败：" + last_error)


def deploy_worker(directory):
    try:
        with locked(directory.parent / "publish.lock"):
            deploy_locked(directory)
    except PublishError as error:
        write_json(directory / "status.json", {"phase": "failed", "updated_at": now(), "error": str(error)})


def deploy_locked(directory):
    request = read_json(directory / "request.json")
    expected = request["target"]
    status = {"release_id": directory.name, "version": request["version"], "updated_at": now()}
    backup = None
    stopped = False
    start_attempted = False

    def phase(value, **fields):
        status.update(phase=value, updated_at=now(), **fields)
        write_json(directory / "status.json", status)

    try:
        phase("preflight")
        target = inspect_target(expected)
        for field in ("container_id", "image_id", "config_sha256", "plugin_sha256", "version",
                      "dataset_id", "state_path", "usage_path", "plugin_dir"):
            require(target[field] == expected[field], "构建期间远端发生变化：" + field)
        for name, digest in request["artifact_hashes"].items():
            require(sha((directory / name).read_bytes()) == digest, "远端候选包校验失败")
        destination = Path(target["plugin_dir"]) / "linux" / "amd64" / request["library"]
        require(not destination.exists(), "目标版本插件文件已存在，拒绝覆盖")
        config_raw = Path(target["config_path"]).read_bytes()
        updated_config = patch_config(config_raw, request["version"])
        phase("stopping")
        command(["docker", "stop", "--time=-1", target["container_id"]], timeout=300)
        require(not docker_inspect(target["container_id"])["State"]["Running"], "CPA 尚未停止")
        stopped = True
        phase("stopped")
        require(sha(Path(target["config_path"]).read_bytes()) == target["config_sha256"], "停机前配置发生变化")
        before, _ = paired_data(target["state_path"], target["usage_path"])
        target["keys"], target["models"] = len(before.get("keys", [])), len(before.get("models", []))
        backup = backup_files(directory, target)
        phase("backed_up", backup=str(backup))
        checker = json.loads(command([directory / "migration-check", "--state", target["state_path"],
                                      "--timezone", target["timezone"]], timeout=120))
        require(checker.get("result") == "passed", "最终落盘数据迁移演练失败")
        phase("switching", migration=checker)
        destination.parent.mkdir(parents=True, exist_ok=True)
        atomic_write(destination, (directory / request["library"]).read_bytes(), 0o755)
        config_path = Path(target["config_path"])
        info = config_path.stat()
        atomic_write(config_path, updated_config, info.st_mode & 0o777)
        os.chown(config_path, info.st_uid, info.st_gid)
        # Once start is attempted, migration or traffic may already have written
        # new data even if Docker/SSH reports an error. Never restore old data.
        phase("starting")
        start_attempted = True
        command(["docker", "start", target["container_id"]], timeout=60)
        phase("verifying")
        new_container_path = str(PurePosixPath(target["plugin_dir_container"]) / "linux" / "amd64" / request["library"])
        result = verify_runtime(target, request["version"], new_container_path)
        require(result["ui_sha256"] == request["ui_sha256"], "远端内嵌页面与候选包不一致")
        after, usage = paired_data(target["state_path"], target["usage_path"])
        require(after["version"] == usage["version"] == checker["target_version"], "实际数据迁移版本不符")
        require(before["dataset_id"] == after["dataset_id"], "实际数据集发生变化")
        verified = json.loads(command([directory / "migration-check", "--state", target["state_path"],
                                       "--timezone", target["timezone"]], timeout=120))
        require(checker.get("policy_sha256") and verified.get("policy_sha256") == checker["policy_sha256"],
                "启动前后 Key 身份、权限、额度、模型或凭证规则发生非预期变化")
        phase("succeeded", verification=result,
              verification_level="runtime_loaded_and_data_checked",
              business_request_tested=False)
    except Exception as error:
        message = str(error) if isinstance(error, PublishError) else type(error).__name__
        if stopped and not start_attempted:
            try:
                if backup:
                    restore_before_start(backup, target)
                else:
                    command(["docker", "start", expected["container_id"]], timeout=60)
                verify_runtime(target, target["version"], target["plugin_container_path"])
                phase("restored", error=message, detail="新版尚未启动，原版本已恢复；未回写任何旧账本")
            except Exception:
                phase("needs_attention", error=message, detail="恢复旧版本未通过验收；保留备份和现场")
        elif start_attempted or status.get("phase") == "stopping":
            phase("needs_attention", error=message, detail="启动或停机结果可能已生效；禁止自动恢复旧账本")
        else:
            phase("failed", error=message)


def remote_main(action, request):
    if action == "inspect":
        return inspect_target(request)
    if action == "stage":
        return remote_stage(request)
    if action == "launch":
        return remote_launch(request)
    if action == "status":
        return remote_status(request)
    raise PublishError("未知远端动作")


def copy_source(root, destination):
    names = command(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=root).decode().split("\0")
    records = {}
    for name in sorted(set(filter(None, names))):
        path = root / name
        if not path.exists():
            continue
        require(path.is_file() and not path.is_symlink(), "发布源码包含符号链接或非文件：" + name)
        raw = path.read_bytes()
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
        target.chmod(path.stat().st_mode & 0o777)
        records[name] = sha(raw)
    require("scripts/publish.py" in records, "publish.py 被 Git 忽略，无法生成完整源码快照")
    return sha(json.dumps(records, sort_keys=True).encode())


def local_preflight(root):
    require(sys.version_info >= (3, 9), "需要 Python 3.9 或更新版本")
    for tool in ("ssh", "git", "node", "npm", "docker", "bash", "go", "rg"):
        require(shutil.which(tool), "本地缺少命令：" + tool)
    require(command(["docker", "info", "--format", "{{.OSType}}"], timeout=20).strip() == b"linux",
            "Docker 必须能运行 Linux 容器")
    require(int(command(["node", "-p", "process.versions.node.split('.')[0]"]).decode()) >= 20, "需要 Node.js 20+")
    go_version = command(["go", "env", "GOVERSION"]).decode().strip()
    match = re.match(r"go(\d+)\.(\d+)", go_version)
    require(match and tuple(map(int, match.groups())) >= (1, 25), "需要 Go 1.25+")
    return source_version(root)


def build_release(root, directory, target, version):
    originals = {name: (root / name).read_bytes() for name in (*VERSION_FILES, EMBED)}
    commit = command(["git", "rev-parse", "HEAD"], cwd=root).decode().strip()
    dirty = bool(command(["git", "status", "--porcelain"], cwd=root).strip())
    library = f"{PLUGIN}-v{version}.so"
    with tempfile.TemporaryDirectory(prefix="cpa-publish-build-", dir=directory) as temporary:
        source = Path(temporary)
        fingerprint = copy_source(root, source)
        bump_source(source, source_version(source), version)
        env = {**os.environ, "CPAKP_SOURCE_COMMIT": commit, "CPAKP_SOURCE_DIRTY": "true",
               "GOFLAGS": "-mod=readonly"}
        print("[2/6] 在源码副本中构建前端并执行现有检查", flush=True)
        command(["make", "web-build"], cwd=source, env=env, show=True, timeout=900)
        for args, cwd in [(["npm", "test"], source / "web"), (["npm", "run", "typecheck"], source / "web"),
                          (["npm", "audit", "--audit-level=moderate"], source / "web"),
                          (["go", "test", "-race", "./..."], source), (["go", "vet", "./..."], source),
                          (["make", "check-version", "check-model-domain"], source)]:
            command(args, cwd=cwd, env=env, show=True, timeout=900)
        print("[3/6] 构建并校验 Linux amd64 插件和迁移检查程序", flush=True)
        if not env.get("CPAKP_GOMODCACHE"):
            cache = command(["go", "env", "GOMODCACHE"], cwd=source, env=env).decode().strip()
            if Path(cache).is_dir():
                env["CPAKP_GOMODCACHE"] = cache
        command(["bash", "scripts/build-linux-amd64.sh", directory / library, directory / "migration-check"],
                cwd=source, env=env, show=True, timeout=1800)
        for name, raw in originals.items():
            require((root / name).read_bytes() == raw, "构建期间本地文件已变化，保留候选产物但不发布：" + name)
        # Reserve the version only after a successful build. Failed deployment can
        # reuse this immutable candidate with --resume without another increment.
        updates = {name: (source / name).read_bytes() for name in originals}
        try:
            for name, raw in updates.items():
                atomic_write(root / name, raw, (root / name).stat().st_mode & 0o777)
        except BaseException:
            for name, raw in originals.items():
                if (root / name).read_bytes() == updates[name]:
                    atomic_write(root / name, raw, (root / name).stat().st_mode & 0o777)
            raise
        ui_digest = sha(updates[EMBED])
    atomic_write(directory / "publish.py", PROGRAM_SOURCE)
    artifact_hashes = {name: sha((directory / name).read_bytes()) for name in (library, "migration-check")}
    request = {"release_id": directory.name, "target": target, "version": version,
               "library": library, "artifact_hashes": artifact_hashes, "ui_sha256": ui_digest,
               "source_commit": commit, "source_dirty": True,
               "source_worktree_was_dirty": dirty, "source_input_sha256": fingerprint}
    write_json(directory / "request.json", request)
    return request


def upload_release(ssh, directory, request):
    names = [request["library"], "migration-check", "publish.py", "request.json"]
    hashes = {name: sha((directory / name).read_bytes()) for name in names}
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w") as archive:
        for name in names:
            archive.add(directory / name, arcname=name, recursive=False)
    return ssh_call(ssh, "stage", {"remote_dir": request["target"]["remote_dir"],
                    "release_id": directory.name, "files": hashes}, payload=buffer.getvalue(), timeout=300)


def watch_release(ssh, directory, target, release_id):
    query = {"remote_dir": target["remote_dir"], "release_id": release_id}
    previous = None
    while True:
        result = ssh_call(ssh, "status", query)
        write_json(directory / "remote-status.json", result)
        if result["phase"] != previous:
            print("[6/6] 远端阶段：" + result["phase"], flush=True)
            previous = result["phase"]
        if result["phase"] in TERMINAL:
            if result["phase"] != "succeeded":
                raise PublishError(result.get("error", "发布未完成") + "；" + result.get("detail", "请保留发布目录并检查现场"))
            return result
        time.sleep(3)


def main():
    parser = argparse.ArgumentParser(description="自动递增 minor 版本、构建 Linux 插件并发布到一个 CPA Docker 实例")
    parser.add_argument("--ssh", required=True, help='例如 "ssh -p 22 -i /path/key root@host"，也支持 "ssh my-cpa"')
    parser.add_argument("--remote-dir", required=True, help="远端实例目录，例如 /opt/cpa/grok")
    parser.add_argument("--container", help="自动识别不唯一时指定 CPA 容器")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="只读检查并显示计划，不递增版本、不构建、不上传")
    mode.add_argument("--resume", metavar="RELEASE_ID", help="继续已构建的发布，复用原版本和产物")
    mode.add_argument("--status", metavar="RELEASE_ID", help="只读查询远端发布状态")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    ssh = ssh_arguments(args.ssh)
    query = {"remote_dir": args.remote_dir, "container": args.container}
    if args.status:
        print(json.dumps(ssh_call(ssh, "status", {**query, "release_id": args.status}), ensure_ascii=False, indent=2))
        return
    if args.dry_run:
        local = local_preflight(root)
        target = ssh_call(ssh, "inspect", query)
        print(json.dumps({"action": "dry-run", "local_version": local,
                          "next_version": next_version(local, target["version"]), "target": target,
                          "maintenance": "构建上传后自动停机备份、迁移演练、切换并启动；会有短暂停机",
                          "rollback": "新版启动前失败恢复旧配置；启动后失败保留新账本并停止自动回退"}, ensure_ascii=False, indent=2))
        return
    releases = root / "dist" / "publish"
    releases.mkdir(parents=True, exist_ok=True)
    with locked(releases / ".lock"):
        if args.resume:
            require(bool(re.fullmatch(r"[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}", args.resume)), "无效发布编号")
            directory = releases / args.resume
            request = read_json(directory / "request.json")
            connection = read_json(directory / "connection.json")
            require(connection == {"ssh": args.ssh, "remote_dir": args.remote_dir}, "继续发布必须使用原 SSH 连接和部署目录")
            query = {"remote_dir": request["target"]["remote_dir"], "release_id": directory.name}
            status = ssh_call(ssh, "status", query)
            if status["phase"] in ("missing", "uploading"):
                upload_release(ssh, directory, request)
                status = {"phase": "staged"}
            if status["phase"] == "staged":
                ssh_call(ssh, "launch", query)
        else:
            print("[1/6] 检查本地构建环境和远端实例", flush=True)
            local = local_preflight(root)
            target = ssh_call(ssh, "inspect", query)
            version = next_version(local, target["version"])
            release_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
            directory = releases / release_id
            directory.mkdir(mode=0o700)
            write_json(directory / "connection.json", {"ssh": args.ssh, "remote_dir": args.remote_dir})
            print(f"计划：{target['container']} {target['version']} → {version}；发布编号 {release_id}", flush=True)
            request = build_release(root, directory, target, version)
            print("[4/6] 上传独立候选包并校验 SHA-256", flush=True)
            upload_release(ssh, directory, request)
            print("[5/6] 启动远端发布进程；即使 SSH 断开也会继续记录结果", flush=True)
            query = {"remote_dir": target["remote_dir"], "release_id": directory.name}
            ssh_call(ssh, "launch", query)
        print(f"发布记录：{directory}\n查询或继续时使用 --status / --resume {directory.name}", flush=True)
        result = watch_release(ssh, directory, request["target"], directory.name)
        print(f"发布完成：{request['target']['container']} → {request['version']}\n备份：{result['backup']}\n"
              "已验证运行版本、加载路径、页面和数据；未发送真实模型计费请求。", flush=True)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--_remote":
        try:
            value = remote_main(sys.argv[2], json.loads(base64.b64decode(sys.argv[3])))
            print(json.dumps({"ok": True, "result": value}, ensure_ascii=False))
        except Exception as error:
            message = str(error) if isinstance(error, PublishError) else type(error).__name__
            print(json.dumps({"ok": False, "error": message}, ensure_ascii=False))
    elif len(sys.argv) > 1 and sys.argv[1] == "--_worker":
        deploy_worker(Path(sys.argv[2]))
    else:
        try:
            main()
        except (PublishError, KeyboardInterrupt, OSError) as error:
            print("发布未完成：" + (str(error) or "本地等待已中断；远端可能仍在执行，请查询状态"), file=sys.stderr)
            sys.exit(1)
