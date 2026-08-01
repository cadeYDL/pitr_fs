#!/usr/bin/env python3
"""生产挂载与宿主普通文件系统的 I/O、恢复性能对照基准。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import statistics
import subprocess
import time


SCALES = {
    "small": 4 * 1024,
    "medium": 1024 * 1024,
    "large": 256 * 1024 * 1024,
    "xlarge": 2 * 1024 * 1024 * 1024,
}
SCALE_CN = {"small": "小", "medium": "中", "large": "大", "xlarge": "超大"}
FS_CN = {"native": "普通文件系统", "pitr": "pitrfs"}
PITR_CLI = "pitr"
DOCKER_COMMAND = ["docker"]
OP_CN = {
    "seq_write": "顺序写",
    "rand_write": "随机写",
    "seq_read": "顺序读",
    "rand_read": "随机读",
}
IO_BYTES = {
    "seq_write": {
        scale: size for scale, size in SCALES.items()
    },
    "rand_write": {
        "small": 100 * 4096,
        "medium": 256 * 4096,
        "large": 4096 * 4096,
        "xlarge": 8192 * 4096,
    },
    "seq_read": {
        "small": 64 * 1024 * 1024,
        "medium": 128 * 1024 * 1024,
        "large": 256 * 1024 * 1024,
        "xlarge": 2 * 1024 * 1024 * 1024,
    },
    "rand_read": {
        "small": 10000 * 4096,
        "medium": 4096 * 4096,
        "large": 8192 * 4096,
        "xlarge": 8192 * 4096,
    },
}


def run(command: list[str], *, capture: bool = True) -> str:
    completed = subprocess.run(
        command,
        check=False,
        text=True,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
    )
    if completed.returncode != 0:
        stdout = completed.stdout or ""
        stderr = completed.stderr or ""
        raise RuntimeError(
            f"命令失败({completed.returncode}): {' '.join(command)}\n{stdout}\n{stderr}"
        )
    return completed.stdout.strip() if capture else ""


def human_size(value: int) -> str:
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:g} {unit}"
        value /= 1024
    raise AssertionError("unreachable")


def fio_one(
    container: str,
    fs_name: str,
    scale: str,
    size: int,
    operation: str,
) -> dict[str, float | int | str]:
    directory = "/native" if fs_name == "native" else "/pitr"
    filename = f"{directory}/fio-{scale}.bin"
    is_random = operation.startswith("rand")
    is_read = operation.endswith("read")
    block_size = 4096 if is_random else min(size, 1024 * 1024)
    rw = {
        "seq_write": "write",
        "rand_write": "randwrite",
        "seq_read": "read",
        "rand_read": "randread",
    }[operation]
    command = [
        *DOCKER_COMMAND, "exec", container, "fio",
        f"--name={fs_name}-{scale}-{operation}",
        f"--filename={filename}",
        f"--size={size}",
        f"--rw={rw}",
        f"--bs={block_size}",
        "--ioengine=psync",
        "--iodepth=1",
        "--direct=1",
        f"--io_size={IO_BYTES[operation][scale]}",
        "--group_reporting=1",
        "--randrepeat=1",
        "--norandommap=1",
        "--fallocate=none",
        "--output-format=json",
    ]
    if not is_read:
        command.insert(-1, "--end_fsync=1")
    payload = json.loads(run(command))
    job = payload["jobs"][0]
    metrics = job["read" if is_read else "write"]
    percentiles = metrics.get("clat_ns", {}).get("percentile", {})
    return {
        "filesystem": fs_name,
        "scale": scale,
        "size_bytes": size,
        "operation": operation,
        "block_size": block_size,
        "bw_mib_s": metrics["bw_bytes"] / 1024 / 1024,
        "iops": metrics["iops"],
        "p99_ms": percentiles.get("99.000000", 0) / 1_000_000,
        "io_bytes": metrics["io_bytes"],
        "runtime_ms": metrics["runtime"],
    }


def write_json(path: Path, rows: list[dict]) -> None:
    path.write_text(
        json.dumps(rows, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )


def io_benchmark(container: str, rounds: int, checkpoint: Path) -> list[dict]:
    raw: list[dict] = []
    operations = ("seq_write", "rand_write", "seq_read", "rand_read")
    for round_number in range(1, rounds + 1):
        filesystems = ("native", "pitr") if round_number % 2 else ("pitr", "native")
        for scale, size in SCALES.items():
            for fs_name in filesystems:
                for operation in operations:
                    print(
                        f"[I/O] round={round_number}/{rounds} "
                        f"fs={fs_name} scale={scale} op={operation}",
                        flush=True,
                    )
                    row = fio_one(container, fs_name, scale, size, operation)
                    row["round"] = round_number
                    raw.append(row)
                    write_json(checkpoint, raw)
    return raw


def median_io(raw: list[dict]) -> list[dict]:
    grouped: dict[tuple[str, str, str], list[dict]] = {}
    for row in raw:
        key = (row["filesystem"], row["scale"], row["operation"])
        grouped.setdefault(key, []).append(row)
    result = []
    for (filesystem, scale, operation), rows in sorted(grouped.items()):
        result.append({
            "filesystem": filesystem,
            "scale": scale,
            "operation": operation,
            "size_bytes": rows[0]["size_bytes"],
            "block_size": rows[0]["block_size"],
            "bw_mib_s": statistics.median(row["bw_mib_s"] for row in rows),
            "iops": statistics.median(row["iops"] for row in rows),
            "p99_ms": statistics.median(row["p99_ms"] for row in rows),
        })
    return result


def write_small(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")


def marker_hash(marker: Path) -> str:
    os.sync()
    time.sleep(0.1)
    output = run([PITR_CLI, "logs", str(marker), "-n", "1"])
    first_line = output.splitlines()[0]
    return first_line.split()[0]


def reset_directory(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True)


def time_revert(version: str, scope: Path) -> tuple[float, str]:
    dry_run = run([PITR_CLI, "revert", version, "--path", str(scope), "--dry-run"])
    started = time.perf_counter_ns()
    output = run([PITR_CLI, "revert", version, "--path", str(scope)])
    elapsed_ms = (time.perf_counter_ns() - started) / 1_000_000
    return elapsed_ms, f"{dry_run} | {output}"


def scenario_single(root: Path, round_number: int) -> dict:
    scope = root / f"single-{round_number}"
    reset_directory(scope)
    data = scope / "large.bin"
    run(["dd", "if=/dev/zero", f"of={data}", "bs=1M", "count=256", "status=none"])
    with data.open("r+b", buffering=0) as stream:
        stream.write(b"baseline-v1")
    marker = scope / ".baseline"
    write_small(marker, "single-baseline")
    version = marker_hash(marker)
    with data.open("r+b", buffering=0) as stream:
        stream.seek(0)
        stream.write(b"changed-v2!")
        stream.seek(-16, os.SEEK_END)
        stream.write(b"changed-at-end!!")
    elapsed_ms, detail = time_revert(version, scope)
    with data.open("rb", buffering=0) as stream:
        prefix = stream.read(11)
        stream.seek(-16, os.SEEK_END)
        suffix = stream.read(16)
    assert prefix == b"baseline-v1", prefix
    assert suffix == bytes(16), suffix
    assert data.stat().st_size == 256 * 1024 * 1024
    return {
        "scenario": "single_file_256mib",
        "round": round_number,
        "revert_ms": elapsed_ms,
        "expected_files": 2,
        "changed_objects": 1,
        "verified": True,
        "detail": detail,
    }


def scenario_multi(root: Path, round_number: int) -> dict:
    scope = root / f"multi-{round_number}"
    reset_directory(scope)
    count = 1000
    for index in range(count):
        write_small(scope / f"f-{index:04d}.txt", f"base-{index:04d}\n")
    marker = scope / ".baseline"
    write_small(marker, "multi-baseline")
    version = marker_hash(marker)
    for index in range(count):
        write_small(scope / f"f-{index:04d}.txt", f"changed-{index:04d}\n")
    elapsed_ms, detail = time_revert(version, scope)
    for index in range(count):
        assert (scope / f"f-{index:04d}.txt").read_text() == f"base-{index:04d}\n"
    return {
        "scenario": "multi_file_1000",
        "round": round_number,
        "revert_ms": elapsed_ms,
        "expected_files": count + 1,
        "changed_objects": count,
        "verified": True,
        "detail": detail,
    }


def scenario_mixed(root: Path, round_number: int) -> dict:
    scope = root / f"mixed-{round_number}"
    reset_directory(scope)
    count = 600
    for index in range(count):
        write_small(scope / f"base-{index:04d}.txt", f"base-{index:04d}\n")
    marker = scope / ".baseline"
    write_small(marker, "mixed-baseline")
    version = marker_hash(marker)
    for index in range(200):
        write_small(scope / f"base-{index:04d}.txt", f"changed-{index:04d}\n")
    for index in range(200, 400):
        (scope / f"base-{index:04d}.txt").unlink()
    for index in range(200):
        write_small(scope / f"new-{index:04d}.txt", f"new-{index:04d}\n")
    elapsed_ms, detail = time_revert(version, scope)
    for index in range(count):
        assert (scope / f"base-{index:04d}.txt").read_text() == f"base-{index:04d}\n"
    assert not list(scope.glob("new-*.txt"))
    return {
        "scenario": "mixed_modify_delete_create_600",
        "round": round_number,
        "revert_ms": elapsed_ms,
        "expected_files": count + 1,
        "changed_objects": 600,
        "verified": True,
        "detail": detail,
    }


def scenario_tree(root: Path, round_number: int) -> dict:
    scope = root / f"tree-{round_number}"
    reset_directory(scope)
    paths: list[Path] = []
    for top in range(10):
        for sub in range(10):
            directory = scope / f"d-{top:02d}" / f"s-{sub:02d}"
            directory.mkdir(parents=True, exist_ok=True)
            for leaf in range(10):
                path = directory / f"f-{leaf:02d}.txt"
                write_small(path, f"base-{top:02d}-{sub:02d}-{leaf:02d}\n")
                paths.append(path)
    marker = scope / ".baseline"
    write_small(marker, "tree-baseline")
    version = marker_hash(marker)
    for index, path in enumerate(paths[:400]):
        write_small(path, f"changed-{index:04d}\n")
    for path in paths[400:700]:
        path.unlink()
    for index in range(300):
        directory = scope / "new-tree" / f"n-{index // 30:02d}"
        directory.mkdir(parents=True, exist_ok=True)
        write_small(directory / f"f-{index:04d}.txt", f"new-{index:04d}\n")
    elapsed_ms, detail = time_revert(version, scope)
    for top in range(10):
        for sub in range(10):
            for leaf in range(10):
                path = scope / f"d-{top:02d}" / f"s-{sub:02d}" / f"f-{leaf:02d}.txt"
                assert path.read_text() == f"base-{top:02d}-{sub:02d}-{leaf:02d}\n"
    assert not (scope / "new-tree").exists()
    return {
        "scenario": "multi_folder_tree_1000",
        "round": round_number,
        "revert_ms": elapsed_ms,
        "expected_files": 1001,
        "changed_objects": 1000,
        "verified": True,
        "detail": detail,
    }


def recovery_benchmark(root: Path, rounds: int, checkpoint: Path) -> list[dict]:
    scenarios = (scenario_single, scenario_multi, scenario_mixed, scenario_tree)
    rows: list[dict] = []
    for round_number in range(1, rounds + 1):
        for scenario in scenarios:
            print(
                f"[恢复] round={round_number}/{rounds} scenario={scenario.__name__}",
                flush=True,
            )
            rows.append(scenario(root, round_number))
            write_json(checkpoint, rows)
    return rows


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def build_report(
    io_rows: list[dict],
    recovery_rows: list[dict],
    rounds: int,
    pitr_mount: str,
    native_path: str,
    native_filesystem: str,
) -> str:
    by_key = {(r["filesystem"], r["scale"], r["operation"]): r for r in io_rows}
    lines = [
        "# pitrfs I/O 与恢复性能实测",
        "",
        "## 测试口径",
        "",
        f"- I/O 独立 {rounds} 轮取中位数；fio psync、iodepth=1、direct=1。",
        "- 顺序写每轮只完整写入一次文件；随机写使用固定 I/O 数，避免版本化文件系统被无限循环覆盖。",
        "- 小/中/大/超大随机写分别为 100/256/4096/8192 次 4 KiB I/O。",
        f"- 普通文件系统为宿主 `{native_path}`（`{native_filesystem}`）；pitrfs 为 `{pitr_mount}`。",
        "- 顺序 block 最大 1 MiB，随机 block 4 KiB；p99 为完成延迟。",
        "- 恢复耗时包含 `pitr` CLI 与 RPC 开销，并逐文件校验结果。",
        "",
        "## I/O 中位数",
        "",
        "| 规模 | 操作 | 普通 MiB/s | pitrfs MiB/s | pitrfs/普通 | 普通 p99 ms | pitrfs p99 ms |",
        "|---|---|---:|---:|---:|---:|---:|",
    ]
    for scale in SCALES:
        for operation in ("seq_write", "rand_write", "seq_read", "rand_read"):
            native = by_key[("native", scale, operation)]
            pitr = by_key[("pitr", scale, operation)]
            ratio = pitr["bw_mib_s"] / native["bw_mib_s"] if native["bw_mib_s"] else 0
            lines.append(
                f"| {SCALE_CN[scale]} ({human_size(SCALES[scale])}) | {OP_CN[operation]} | "
                f"{native['bw_mib_s']:.2f} | {pitr['bw_mib_s']:.2f} | {ratio:.2%} | "
                f"{native['p99_ms']:.3f} | {pitr['p99_ms']:.3f} |"
            )
    lines += [
        "",
        "## 恢复性能",
        "",
        "| 场景 | 变更对象 | 中位数 ms | 最小 ms | 最大 ms | 校验 |",
        "|---|---:|---:|---:|---:|:---:|",
    ]
    labels = {
        "single_file_256mib": "单个 256 MiB 文件",
        "multi_file_1000": "1000 文件全部修改",
        "mixed_modify_delete_create_600": "600 文件：修改/删除/新增",
        "multi_folder_tree_1000": "多层目录树 1000 文件",
    }
    for scenario, label in labels.items():
        selected = [row for row in recovery_rows if row["scenario"] == scenario]
        values = [row["revert_ms"] for row in selected]
        verified = all(row["verified"] for row in selected)
        lines.append(
            f"| {label} | {selected[0]['changed_objects']} | {statistics.median(values):.2f} | "
            f"{min(values):.2f} | {max(values):.2f} | {'通过' if verified else '失败'} |"
        )
    lines += ["", "## 原始恢复输出", ""]
    for row in recovery_rows:
        lines.append(
            f"- `{row['scenario']}` 第 {row['round']} 轮：{row['revert_ms']:.2f} ms；"
            f"`{row['detail']}`"
        )
    return "\n".join(lines) + "\n"


def current_history_limit() -> int:
    """读取 config 的稳定制表符输出，避免覆盖用户原有配置。"""
    output = run([PITR_CLI, "config"])
    for line in output.splitlines():
        columns = line.split("\t")
        if columns and columns[0] == "history-limit" and len(columns) >= 2:
            return int(columns[1])
    raise RuntimeError("无法从 pitr config 读取 history-limit")


def set_history_limit(value: int) -> None:
    run([PITR_CLI, "config", "set", "history-limit", str(value)])


def restore_history_limit(current: int, target: int, step: int = 1000) -> None:
    """分批淘汰版本，避免一次清理过多历史超过 CLI RPC 超时。"""
    if current <= target:
        if current != target:
            set_history_limit(target)
        return
    while current > target:
        next_limit = max(target, current - step)
        try:
            set_history_limit(next_limit)
        except RuntimeError:
            # RPC 响应丢失时配置可能已经提交，先查询再决定是否重试。
            observed = current_history_limit()
            if observed > next_limit:
                raise
            next_limit = observed
        current = next_limit


def cleanup_benchmark_root(cleanup_root: Path, pitr_mount: Path) -> None:
    """只允许删除本入口创建的挂载点直属测试目录。"""
    resolved_root = cleanup_root.resolve()
    resolved_mount = pitr_mount.resolve()
    if resolved_root.parent != resolved_mount:
        raise RuntimeError(f"拒绝清理非挂载点直属目录: {resolved_root}")
    if not resolved_root.name.startswith("__pitr_bench_"):
        raise RuntimeError(f"拒绝清理非基准目录: {resolved_root}")
    if resolved_root.exists():
        shutil.rmtree(resolved_root)


def main() -> None:
    global PITR_CLI, DOCKER_COMMAND
    parser = argparse.ArgumentParser()
    parser.add_argument("--container", required=True, help="由运行入口创建的 fio 容器名")
    parser.add_argument(
        "--docker-command",
        default="docker",
        help="Docker 命令；当前用户无权限时可传 'sudo docker'",
    )
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--output", required=True)
    parser.add_argument("--recovery-root", required=True)
    parser.add_argument(
        "--cleanup-root",
        help="恢复历史上限前删除入口创建的测试目录",
    )
    parser.add_argument("--native-path", required=True)
    parser.add_argument("--pitr-cli", default="pitr")
    parser.add_argument("--pitr-mount", required=True)
    parser.add_argument("--benchmark-history-limit", type=int, default=5000)
    parser.add_argument(
        "--restore-history-limit",
        type=int,
        help="结束时恢复到指定值；默认自动读取运行前的值",
    )
    parser.add_argument("--keep-history-limit", action="store_true")
    parser.add_argument(
        "--skip-io",
        action="store_true",
        help="复用输出目录中的 io-median.json，仅续跑恢复测试",
    )
    args = parser.parse_args()
    PITR_CLI = args.pitr_cli
    DOCKER_COMMAND = shlex.split(args.docker_command)
    if not DOCKER_COMMAND:
        parser.error("--docker-command 不能为空")
    if args.rounds < 1:
        parser.error("--rounds 必须大于 0")
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    original_history_limit = current_history_limit()
    target_history_limit = (
        args.restore_history_limit
        if args.restore_history_limit is not None
        else original_history_limit
    )
    benchmark_history_limit = max(
        original_history_limit, args.benchmark_history_limit
    )
    if benchmark_history_limit != original_history_limit:
        set_history_limit(benchmark_history_limit)
    cleanup_error: Exception | None = None
    try:
        if args.skip_io:
            io_rows = json.loads((output / "io-median.json").read_text(encoding="utf-8"))
        else:
            io_raw = io_benchmark(args.container, args.rounds, output / "io-raw.json")
            io_rows = median_io(io_raw)
            write_json(output / "io-median.json", io_rows)
        recovery_rows = recovery_benchmark(
            Path(args.recovery_root), args.rounds, output / "recovery-raw.json"
        )
    finally:
        try:
            if args.cleanup_root:
                cleanup_benchmark_root(
                    Path(args.cleanup_root),
                    Path(args.pitr_mount),
                )
        except Exception as error:  # 恢复配置优先，清理错误稍后再抛出。
            cleanup_error = error
        finally:
            if not args.keep_history_limit:
                restore_history_limit(
                    benchmark_history_limit,
                    target_history_limit,
                )
    if cleanup_error is not None:
        raise RuntimeError(f"测试数据清理失败: {cleanup_error}") from cleanup_error
    write_json(output / "recovery-raw.json", recovery_rows)
    native_filesystem = run(
        ["findmnt", "-T", args.native_path, "-n", "-o", "SOURCE,FSTYPE"]
    )
    report = build_report(
        io_rows,
        recovery_rows,
        args.rounds,
        args.pitr_mount,
        args.native_path,
        native_filesystem,
    )
    (output / "REPORT.md").write_text(report, encoding="utf-8")
    manifest = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "rounds": args.rounds,
        "native_path": args.native_path,
        "native_mount": native_filesystem,
        "pitr_mount": run(["findmnt", "-T", args.pitr_mount, "-n", "-o", "SOURCE,FSTYPE"]),
        "docker_command": DOCKER_COMMAND,
        "fio_container": args.container,
        "kernel": run(["uname", "-sr"]),
        "script_sha256": sha256_file(Path(__file__)),
    }
    (output / "environment.json").write_text(
        json.dumps(manifest, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    print(report)


if __name__ == "__main__":
    main()
