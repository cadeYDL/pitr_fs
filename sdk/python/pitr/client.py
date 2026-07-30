"""pitrd gRPC 控制面的 Python 语义封装。"""

from __future__ import annotations

import os
from contextlib import contextmanager
from dataclasses import dataclass
from threading import Lock
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


@dataclass(frozen=True)
class RevertResult:
    applied: int
    new_version_hash: str


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
    retention: str
    error: str


class Transaction:
    """一个 active PITR transaction；commit/rollback 至多成功一次。"""

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
        self._lock = Lock()

    def commit(self, message: str = "", timeout: float | None = None) -> None:
        with self._lock:
            self._require_active()
            response = self._client._stub.Commit(
                pb.CommitRequest(txn_id=self.txn_id, message=message),
                timeout=timeout,
            )
            self.state = response.transaction.state

    def rollback(self, timeout: float | None = None) -> None:
        with self._lock:
            self._require_active()
            response = self._client._stub.Rollback(
                pb.RollbackRequest(txn_id=self.txn_id),
                timeout=timeout,
            )
            self.state = response.transaction.state

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
        resolved = _resolve_path(path)
        response = self._stub.Begin(
            pb.BeginRequest(path=resolved, message=message),
            timeout=timeout,
        )
        value = response.transaction
        return Transaction(
            self,
            value.scope_path,
            value.version_hash,
            value.txn_id,
            value.state,
        )

    @contextmanager
    def transaction(
        self,
        path: str,
        message: str = "",
        *,
        commit_message: str = "",
        timeout: float | None = None,
    ) -> Iterator[Transaction]:
        value = self.begin(path, message, timeout)
        try:
            yield value
        except BaseException:
            value.rollback(timeout)
            raise
        else:
            value.commit(commit_message, timeout)

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
            )

    def revert(
        self,
        version_hash: str,
        *,
        path: str = "",
        dry_run: bool = False,
        timeout: float | None = None,
    ) -> RevertResult:
        resolved = _resolve_path(path)
        response = self._stub.Revert(
            pb.RevertRequest(
                version_hash=version_hash,
                path=resolved,
                dry_run=dry_run,
            ),
            timeout=timeout,
        )
        return RevertResult(response.applied, response.new_version_hash)

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
                value.name,
                value.jfs_mount,
                value.fuse_mount,
                value.jfs_mounted,
                value.fuse_mounted,
                value.retention,
                value.error,
            )
            for value in response.volumes
        ]
