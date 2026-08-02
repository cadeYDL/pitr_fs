"""pitrd gRPC 控制面的 Python 语义封装。"""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime
from typing import Iterator

import grpc

from .pb import pitrd_pb2 as pb
from .pb import pitrd_pb2_grpc as rpc


def _resolve_path(path: str) -> str:
    """按客户端当前工作目录解析路径；空值继续表示全局范围。"""
    if not path:
        return ""
    return os.path.abspath(path)


@dataclass(frozen=True)
class LogEntry:
    txn_id: int
    version_hash: str
    scope_path: str
    state: str
    command: str
    message: str
    created_at: datetime | None
    closed_at: datetime | None
    posix_operation: str
    process_command: str
    actor_uid: int
    actor_gid: int
    actor_pid: int
    actor_name: str
    change_summary: str


@dataclass(frozen=True)
class RevertResult:
    applied: int
    new_version_hash: str
    resolved_version_hash: str
    resolved_version_time: str


@dataclass(frozen=True)
class DiffStats:
    node_changes: int
    edge_changes: int
    chunk_changes: int


@dataclass(frozen=True)
class Volume:
    name: str
    jfs_mount: str
    fuse_mount: str
    jfs_mounted: bool
    fuse_mounted: bool
    history_limit: int
    error: str
    max_space_bytes: int = 0
    space_reserve_percent: int = 20
    retained_space_bytes: int = 0
    reclaimable_space_bytes: int = 0


@dataclass(frozen=True)
class VersionSpace:
    version_hash: str
    closed_at: str
    pinned_bytes: int
    releasable_bytes: int


@dataclass(frozen=True)
class SpaceInfo:
    max_bytes: int
    reserve_percent: int
    high_watermark_bytes: int
    retained_bytes: int
    reclaimable_bytes: int
    versions: tuple[VersionSpace, ...]


class Transaction:
    """已废弃的兼容类型；自动版本模式不再创建手工事务。"""

    def __init__(
        self,
        client: Client,
        path: str,
        version_hash: str,
        txn_id: int,
        state: str = "active",
    ) -> None:
        self._client = client
        self.path = path
        self.version_hash = version_hash
        self.txn_id = txn_id
        self.state = state
        self._lock = None

    def commit(self, message: str = "", timeout: float | None = None) -> None:
        raise RuntimeError("手工 transaction 已停用：写操作会自动形成版本")

    def rollback(self, timeout: float | None = None) -> None:
        raise RuntimeError("手工 transaction 已停用：请使用 revert")

    def _require_active(self) -> None:
        if self.state != "active":
            raise RuntimeError(f"transaction 已结束: state={self.state}")


class Client:
    def __init__(
        self,
        socket: str = "/var/run/pitrd.sock",
        *,
        channel: grpc.Channel | None = None,
    ) -> None:
        if not socket and channel is None:
            raise ValueError("pitrd socket 不能为空")
        target = socket if "://" in socket else f"unix://{socket}"
        self._channel = channel or grpc.insecure_channel(target)
        self._owns_channel = channel is None
        self._stub = rpc.PitrdStub(self._channel)

    def close(self) -> None:
        if self._owns_channel:
            self._channel.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def begin(
        self,
        path: str,
        message: str = "",
        timeout: float | None = None,
    ) -> Transaction:
        raise RuntimeError(
            "手工 transaction 已停用：写操作会自动形成版本"
        )

    def transaction(
        self,
        path: str,
        message: str = "",
        *,
        commit_message: str = "",
        timeout: float | None = None,
    ) -> Iterator[Transaction]:
        raise RuntimeError(
            "手工 transaction 已停用：写操作会自动形成版本"
        )

    def logs(
        self,
        path: str,
        limit: int = 20,
        timeout: float | None = None,
    ) -> Iterator[LogEntry]:
        resolved = _resolve_path(path)
        response = self._stub.Logs(
            pb.LogsRequest(path=resolved, limit=limit),
            timeout=timeout,
        )
        for entry in response.entries:
            value = entry.transaction
            yield LogEntry(
                txn_id=value.txn_id,
                version_hash=value.version_hash,
                scope_path=value.scope_path,
                state=value.state,
                command=value.command,
                message=value.message,
                created_at=(
                    value.created_at.ToDatetime()
                    if value.HasField("created_at")
                    else None
                ),
                closed_at=(
                    value.closed_at.ToDatetime()
                    if value.HasField("closed_at")
                    else None
                ),
                posix_operation=value.posix_operation,
                process_command=value.process_command,
                actor_uid=value.actor_uid,
                actor_gid=value.actor_gid,
                actor_pid=value.actor_pid,
                actor_name=value.actor_name,
                change_summary=value.change_summary,
            )

    def revert(
        self,
        version_hash: str,
        *,
        path: str = ".",
        global_scope: bool = False,
        dry_run: bool = False,
        timeout: float | None = None,
    ) -> RevertResult:
        if not version_hash.strip():
            raise ValueError("version hash 不能为空")
        return self._revert(
            version_hash=version_hash,
            target_time="",
            path=path,
            global_scope=global_scope,
            dry_run=dry_run,
            timeout=timeout,
        )

    def revert_at(
        self,
        target_time: datetime,
        *,
        path: str = ".",
        global_scope: bool = False,
        dry_run: bool = False,
        timeout: float | None = None,
    ) -> RevertResult:
        if target_time.tzinfo is None or target_time.utcoffset() is None:
            raise ValueError("target_time 必须包含时区")
        return self._revert(
            version_hash="",
            target_time=target_time.isoformat(),
            path=path,
            global_scope=global_scope,
            dry_run=dry_run,
            timeout=timeout,
        )

    def _revert(
        self,
        *,
        version_hash: str,
        target_time: str,
        path: str,
        global_scope: bool,
        dry_run: bool,
        timeout: float | None,
    ) -> RevertResult:
        if global_scope and path != ".":
            raise ValueError("global_scope 与 path 不能同时使用")
        resolved = "" if global_scope else _resolve_path(path)
        response = self._stub.Revert(
            pb.RevertRequest(
                version_hash=version_hash,
                target_time=target_time,
                path=resolved,
                dry_run=dry_run,
            ),
            timeout=timeout,
        )
        return RevertResult(
            response.applied,
            response.new_version_hash,
            response.resolved_version_hash,
            response.resolved_version_time,
        )

    def diff(
        self,
        version_a: str,
        version_b: str,
        *,
        path: str = "",
        timeout: float | None = None,
    ) -> DiffStats:
        resolved = _resolve_path(path)
        response = self._stub.Diff(
            pb.DiffRequest(
                version_a=version_a,
                version_b=version_b,
                path=resolved,
            ),
            timeout=timeout,
        )
        return DiffStats(
            response.node_changes,
            response.edge_changes,
            response.chunk_changes,
        )

    def recover(
        self,
        path: str = "",
        timeout: float | None = None,
    ) -> list[Volume]:
        resolved = _resolve_path(path)
        response = self._stub.Recover(
            pb.RecoverRequest(path=resolved),
            timeout=timeout,
        )
        return [
            Volume(
                name=value.name,
                jfs_mount=value.jfs_mount,
                fuse_mount=value.fuse_mount,
                jfs_mounted=value.jfs_mounted,
                fuse_mounted=value.fuse_mounted,
                history_limit=value.history_limit,
                error=value.error,
                max_space_bytes=value.max_space_bytes,
                space_reserve_percent=value.space_reserve_percent,
                retained_space_bytes=value.retained_space_bytes,
                reclaimable_space_bytes=value.reclaimable_space_bytes,
            )
            for value in response.volumes
        ]

    def set_history_limit(
        self,
        limit: int,
        timeout: float | None = None,
    ) -> None:
        if limit != -1 and limit < 1:
            raise ValueError(f"history limit 必须是 -1 或正整数: {limit}")
        self._stub.ConfigSet(
            pb.ConfigSetRequest(key="history-limit", value=str(limit)),
            timeout=timeout,
        )

    def set_max_space_bytes(
        self,
        max_bytes: int,
        timeout: float | None = None,
    ) -> None:
        if max_bytes < 0:
            raise ValueError(f"max space 不能为负数: {max_bytes}")
        self._stub.ConfigSet(
            pb.ConfigSetRequest(key="max-space", value=f"{max_bytes}B"),
            timeout=timeout,
        )

    def set_space_reserve(
        self,
        percent: int,
        timeout: float | None = None,
    ) -> None:
        if not 1 <= percent <= 99:
            raise ValueError(f"space reserve 必须在 1..99 之间: {percent}")
        self._stub.ConfigSet(
            pb.ConfigSetRequest(key="space-reserve", value=f"{percent}%"),
            timeout=timeout,
        )

    def space(
        self,
        path: str = ".",
        limit: int = 20,
        timeout: float | None = None,
    ) -> SpaceInfo:
        response = self._stub.Space(
            pb.SpaceRequest(path=_resolve_path(path), limit=limit),
            timeout=timeout,
        )
        return SpaceInfo(
            max_bytes=response.max_space_bytes,
            reserve_percent=response.reserve_percent,
            high_watermark_bytes=response.high_watermark_bytes,
            retained_bytes=response.retained_bytes,
            reclaimable_bytes=response.reclaimable_bytes,
            versions=tuple(
                VersionSpace(
                    version_hash=value.version_hash,
                    closed_at=value.closed_at,
                    pinned_bytes=value.pinned_bytes,
                    releasable_bytes=value.estimated_release_bytes,
                )
                for value in response.versions
            ),
        )

    def clear(
        self,
        *,
        confirm: bool = False,
        timeout: float | None = None,
    ) -> tuple[int, int]:
        if not confirm:
            raise ValueError("clear 必须显式 confirm=True")
        response = self._stub.Clear(
            pb.ClearRequest(**{"global": True, "confirm": True}),
            timeout=timeout,
        )
        return response.versions_deleted, response.history_deleted
