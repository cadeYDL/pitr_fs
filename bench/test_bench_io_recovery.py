#!/usr/bin/env python3
"""bench-io-recovery.py 的无环境依赖单元测试。"""

from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest import mock


MODULE_PATH = Path(__file__).with_name("bench-io-recovery.py")
SPEC = importlib.util.spec_from_file_location("bench_io_recovery", MODULE_PATH)
assert SPEC and SPEC.loader
BENCH = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BENCH)


class BenchIORecoveryTest(unittest.TestCase):
    def test_current_history_limit_parses_tabular_config(self) -> None:
        config = (
            "配置项\t当前值\t默认值\t范围\t说明\n"
            "history-limit\t42\t100\t1..100000\t最多保留的版本数\n"
        )
        with mock.patch.object(BENCH, "run", return_value=config):
            self.assertEqual(BENCH.current_history_limit(), 42)

    def test_current_history_limit_rejects_missing_row(self) -> None:
        with mock.patch.object(BENCH, "run", return_value="配置项\t当前值"):
            with self.assertRaisesRegex(RuntimeError, "history-limit"):
                BENCH.current_history_limit()

    def test_fio_uses_configured_docker_command(self) -> None:
        payload = (
            '{"jobs":[{"write":{"bw_bytes":1048576,"iops":1,'
            '"clat_ns":{"percentile":{"99.000000":1000000}},'
            '"io_bytes":4096,"runtime":1}}]}'
        )
        original = BENCH.DOCKER_COMMAND
        BENCH.DOCKER_COMMAND = ["sudo", "docker"]
        try:
            with mock.patch.object(BENCH, "run", return_value=payload) as run:
                row = BENCH.fio_one("fio-test", "native", "small", 4096, "seq_write")
        finally:
            BENCH.DOCKER_COMMAND = original
        self.assertEqual(run.call_args.args[0][:4], ["sudo", "docker", "exec", "fio-test"])
        self.assertEqual(row["bw_mib_s"], 1)

    def test_restore_history_limit_uses_bounded_steps(self) -> None:
        with mock.patch.object(BENCH, "set_history_limit") as setter:
            BENCH.restore_history_limit(5000, 100, step=1000)
        self.assertEqual(
            [call.args[0] for call in setter.call_args_list],
            [4000, 3000, 2000, 1000, 100],
        )

    def test_restore_history_limit_accepts_committed_timeout(self) -> None:
        with (
            mock.patch.object(
                BENCH,
                "set_history_limit",
                side_effect=[RuntimeError("timeout"), None],
            ) as setter,
            mock.patch.object(BENCH, "current_history_limit", return_value=1000),
        ):
            BENCH.restore_history_limit(2000, 100, step=1000)
        self.assertEqual([call.args[0] for call in setter.call_args_list], [1000, 100])

    def test_cleanup_benchmark_root_is_scoped(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mount = Path(directory)
            root = mount / "__pitr_bench_123"
            root.mkdir()
            (root / "sample").write_text("data", encoding="utf-8")
            BENCH.cleanup_benchmark_root(root, mount)
            self.assertFalse(root.exists())

    def test_cleanup_benchmark_root_rejects_other_directory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            mount = Path(directory)
            root = mount / "user-data"
            root.mkdir()
            with self.assertRaisesRegex(RuntimeError, "非基准目录"):
                BENCH.cleanup_benchmark_root(root, mount)
            self.assertTrue(root.exists())


if __name__ == "__main__":
    unittest.main()
