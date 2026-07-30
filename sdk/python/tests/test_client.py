from __future__ import annotations

from concurrent import futures
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

    @staticmethod
    def _transaction(state: str, command: str) -> pb.Transaction:
        return pb.Transaction(
            txn_id=7,
            version_hash="012345abcdef",
            scope_path="/workspace/proj",
            state=state,
            command=command,
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
        return pb.RevertResponse(applied=3, new_version_hash="fedcba654321")

    def Diff(self, request, context):  # noqa: N802
        return pb.DiffResponse(node_changes=1, edge_changes=2, chunk_changes=3)


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
        transaction = client.begin("/workspace/proj", "edit", timeout=2)
        assert transaction.version_hash == "012345abcdef"
        assert transaction.txn_id == 7
        assert transaction.path == "/workspace/proj"


def test_begin_resolves_relative_path(pitrd, tmp_path: Path, monkeypatch):
    socket, _ = pitrd
    monkeypatch.chdir(tmp_path)
    with Client(socket) as client:
        transaction = client.begin("project/../project")
    assert transaction.path == str(tmp_path / "project")


def test_with_transaction_commits_on_success(pitrd):
    socket, implementation = pitrd
    with Client(socket) as client:
        with client.transaction("/workspace/proj", commit_message="done"):
            pass
    assert implementation.commits == 1
    assert implementation.rollbacks == 0


def test_with_transaction_rolls_back_on_exception(pitrd):
    socket, implementation = pitrd
    with pytest.raises(RuntimeError, match="boom"):
        with Client(socket) as client:
            with client.transaction("/workspace/proj"):
                raise RuntimeError("boom")
    assert implementation.commits == 0
    assert implementation.rollbacks == 1


def test_logs_iteration(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        values = list(client.logs("/workspace/proj", 2))
    assert [value.command for value in values] == ["write:a", "commit"]


def test_revert_with_path_and_diff(pitrd):
    socket, _ = pitrd
    with Client(socket) as client:
        reverted = client.revert("111111111111", path="/workspace/proj")
        diff = client.diff(
            "111111111111", "222222222222", path="/workspace/proj"
        )
    assert reverted.applied == 3
    assert reverted.new_version_hash == "fedcba654321"
    assert diff.node_changes == 1
    assert diff.edge_changes == 2
    assert diff.chunk_changes == 3
