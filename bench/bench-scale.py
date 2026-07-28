#!/usr/bin/env python3
"""
维度 1:规模承载 —— 元数据延迟随 inode 数量变化

对三挡分别跑:每一批 create N 个小文件,记录 create/stat/unlink 平均延迟,
观察是否随规模退化(拐点)。
"""
import argparse, os, random, sys, time
from pathlib import Path

BATCHES = [1000, 5000, 10000, 50000, 100000]   # 累积到多少 inode 时采样
STAT_SAMPLE = 1000

def bench(mnt: Path, tag: str):
    target = mnt / "scale"
    if target.exists():
        # 清空
        for f in target.iterdir():
            f.unlink(missing_ok=True)
    else:
        target.mkdir()

    rows = []
    total = 0
    for target_n in BATCHES:
        to_create = target_n - total
        t0 = time.time()
        for i in range(to_create):
            (target / f"f_{total+i}").write_text("x")
        create_ms = (time.time() - t0) / to_create * 1000

        sample = random.sample(range(target_n), min(STAT_SAMPLE, target_n))
        t0 = time.time()
        for i in sample:
            (target / f"f_{i}").stat()
        stat_ms = (time.time() - t0) / len(sample) * 1000

        rows.append((tag, target_n, round(create_ms, 3), round(stat_ms, 3)))
        print(f"  {tag:6s}  n={target_n:>7d}  create={create_ms:6.3f} ms/op  stat={stat_ms:6.3f} ms/op")
        total = target_n
    return rows

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mnts", nargs="+", required=True, help="tag=path,tag=path,...")
    ap.add_argument("--out",  required=True)
    args = ap.parse_args()

    all_rows = []
    for spec in args.mnts:
        tag, path = spec.split("=", 1)
        print(f"==> {tag} @ {path}")
        all_rows += bench(Path(path), tag)

    import json
    Path(args.out).write_text(json.dumps(all_rows))
    print(f"\nresults → {args.out}")

if __name__ == "__main__":
    main()
