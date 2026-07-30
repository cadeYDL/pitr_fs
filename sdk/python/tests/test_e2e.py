from __future__ import annotations

import os
from pathlib import Path

import pytest

from pitr import Client


def test_python_sdk_real_pitrd_e2e():
    socket = os.getenv("PITR_E2E_SOCKET")
    host_path_value = os.getenv("PITR_E2E_HOST_PATH")
    scope = os.getenv("PITR_E2E_SCOPE")
    if not socket or not host_path_value or not scope:
        pytest.skip(
            "未设置 PITR_E2E_SOCKET/PITR_E2E_HOST_PATH/PITR_E2E_SCOPE"
        )

    host_path = Path(host_path_value)
    host_path.mkdir(parents=True, exist_ok=True)
    file_path = host_path / "file.txt"
    with Client(socket) as client:
        with client.transaction(
            scope, "python-sdk-v1", commit_message="python-sdk-v1"
        ) as v1:
            file_path.write_text("python-v1")
        with client.transaction(
            scope, "python-sdk-v2", commit_message="python-sdk-v2"
        ):
            file_path.write_text("python-v2")

        reverted = client.revert(v1.version_hash, path=scope)
        assert reverted.applied > 0
        assert file_path.read_text() == "python-v1"

        with pytest.raises(RuntimeError, match="force rollback"):
            with client.transaction(scope, "python-sdk-rollback"):
                file_path.write_text("must-rollback")
                raise RuntimeError("force rollback")
        assert file_path.read_text() == "python-v1"
        assert len(list(client.logs(scope, 20))) >= 4
