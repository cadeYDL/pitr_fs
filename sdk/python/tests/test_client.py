from __future__ import annotations

from concurrent import futures
from datetime import datetime, timedelta, timezone
from pathlib import Path

import grpc
import pytest

from pitr import Client
from pitr.pb import pitrd_pb2 as pb
from pitr.pb import pitrd_pb2_grpc as rpc


class FakePitrd(rpc.PitrdServicer):
    def __init__(self) -> None:
        self.commits = 0
        self.rollbacks = 0
        self.last_revert_path = None
        self.last_revert_time = None

    @staticmethod
    def _transaction(state: str, command: str) -> pb.Transaction:
        return pb.Transaction(
            txn_id=7,
            version_hash="012345abcdef",
            scope_path="/workspace/proj",
            state=state,
            command=command,
            posix_operation="write(file, 2)",
            process_command="echo hi > file",
            actor_uid=1000,
            actor_gid=1000,
            actor_pid=22,
            actor_name="tester",
            change_summary='"v1" -> "v2"',
        )

    def Begin(self, request, context):  # noqa: N802
        value = self._transaction("active", "begin")
        value.scope_path = request.path
        value.message = request.message
        return pb.BeginResponse(transaction=value)

    def Commit(self, request, context):  # noqa: N802
        self.commits += 1
        return pb.CommitResponse(transaction=self._transaction("committed", "commit"))

    def Rollback(self, request, context):  # noqa: N802
        self.rollbacks += 1
        return pb.RollbackResponse(
            transaction=self._transaction("rolled_back", "rollback")
        )

    def Logs(self, request, context):  # noqa: N802
        return pb.LogsResponse(
            entries=[
                pb.LogEntry(transaction=self._transaction("auto", "write:a")),
                pb.LogEntry(transaction=self._transaction("committed", "commit")),
            ]
        )

    def Revert(self, request, context):  # noqa: N802
        self.last_revert_path = request.path
        self.last_revert_time = request.target_time
        return pb.RevertResponse(
            applied=3,
            new_version_hash="fedcba654321",
            resolved_version_hash="111111111111",
            resolved_version_time="2026-07-31T10:00:00Z",
        )

    def Diff(self, request, context):  # noqa: N802
        return pb.DiffResponse(node_changes=1, edge_changes=2, chunk_changes=3)

    def ConfigSet(self, request, context):  # noqa: N802
        return pb.ConfigSetResponse(key=request.key, value=request.value)

    def Clear(self, request, context):  # noqa: N802
        return pb.ClearResponse(versions_deleted=4, history_deleted=12)

    def Squash(self, request, context):  # noqa: N802
        return pb.SquashResponse(
            base_version=request.base_version,
            end_version=request.end_version,
            versions_merged=3,
            versions_deleted=2,
            history_before=8,
            history_after=3,
            history_deleted=5,
            first_operation_at="2026-07-31T10:00:00Z",
            end_closed_at="2026-07-31T10:01:00Z",
            dry_run=request.dry_run,
        )

    def Space(self, request, context):  # noqa: N802
        return pb.SpaceResponse(
            max_space_bytes=100 << 30,
            reserve_percent=20,
            high_watermark_bytes=80 << 30,
            retained_bytes=60 << 30,
            versions=[
                pb.SpaceVersion(
                    version_hash="012345abcdef",
                    pinned_bytes=2 << 30,
                    estimated_release_bytes=1 << 30,
                )
            ],
        )


@pytest.fixture
def pitrd(tmp_path: Path):
    socket = tmp_path / "pitrd.sock"
    implementation = FakePitrd()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    rpc.add_PitrdServicer_to_server(implementation, server)
    server.add_insecure_port(f"unix://{socket}")
    server.start()
    try:
        yield str(socket), implementation
    finally:
        server.stop(0).wait()


def test_client_connect(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        with pytest.raises(RuntimeError, match="手工 transaction 已停用"):
            client.begin("/workspace/proj", "edit", timeout=2)


def test_revert_defaults_to_current_path(pitrd, tmp_path: Path, monkeypatch):
    socket, implementation = pitrd
    monkeypatch.chdir(tmp_path)
    with Client(socket) as client:
        client.revert("111111111111")
        assert implementation.last_revert_path == str(tmp_path)
        client.revert("111111111111", global_scope=True)
        assert implementation.last_revert_path == ""


def test_transaction_context_disabled(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        with pytest.raises(RuntimeError, match="手工 transaction 已停用"):
            with client.transaction("/workspace/proj"):
                pass


def test_logs_iteration(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        values = list(client.logs("/workspace/proj", 2))
    assert [value.command for value in values] == ["write:a", "commit"]
    assert values[0].posix_operation == "write(file, 2)"
    assert values[0].process_command == "echo hi > file"
    assert values[0].actor_name == "tester"
    assert values[0].change_summary == '"v1" -> "v2"'


def test_revert_with_path_and_diff(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        reverted = client.revert("111111111111", path="/workspace/proj")
        diff = client.diff(
            "111111111111", "222222222222", path="/workspace/proj"
        )
    assert reverted.applied == 3
    assert reverted.new_version_hash == "fedcba654321"
    assert reverted.resolved_version_hash == "111111111111"
    assert diff.node_changes == 1
    assert diff.edge_changes == 2
    assert diff.chunk_changes == 3


def test_revert_at_requires_timezone_and_sends_timestamp(pitrd):
    socket, implementation = pitrd
    target = datetime(
        2026, 7, 31, 18, 0, 0, 123, tzinfo=timezone(timedelta(hours=8))
    )
    with Client(socket) as client:
        result = client.revert_at(target, path="/workspace/proj")
        with pytest.raises(ValueError, match="包含时区"):
            client.revert_at(datetime(2026, 7, 31, 18, 0, 0))
        with pytest.raises(ValueError, match="不能为空"):
            client.revert("")
    assert implementation.last_revert_time == target.isoformat()
    assert result.resolved_version_hash == "111111111111"


def test_config_and_clear(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        client.set_history_limit(12)
        client.set_history_limit(-1)
        with pytest.raises(ValueError, match="-1 或正整数"):
            client.set_history_limit(0)
        client.set_max_space_bytes(100 << 30)
        client.set_space_reserve(20)
        space = client.space("/workspace", 10)
        assert space.high_watermark_bytes == 80 << 30
        assert space.versions[0].releasable_bytes == 1 << 30
        with pytest.raises(ValueError, match="confirm=True"):
            client.clear()
        assert client.clear(confirm=True) == (4, 12)


def test_squash(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        preview = client.squash(
            "111111111111", "222222222222", "发布功能", dry_run=True
        )
        assert preview.dry_run is True
        assert preview.versions_merged == 3
        assert preview.history_deleted == 5
        with pytest.raises(ValueError, match="必须且只能"):
            client.squash(
                "111111111111",
                "222222222222",
                "发布功能",
                dry_run=False,
                confirm=False,
            )
