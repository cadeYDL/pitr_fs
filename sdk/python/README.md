# PITR-FS Python SDK

```python
from pitr import Client

with Client("/var/run/pitrd.sock") as client:
    with open("/workspace/project/result.txt", "w") as file:
        file.write("done\n")

    # 文件关闭后已经自动形成版本。
    versions = list(client.logs("/workspace/project", 20))
    client.revert(
        versions[0].version_hash,
        path="/workspace/project",
    )
```

`revert` 默认作用于当前目录，整卷回退需传 `global_scope=True`。通过
`set_history_limit(100)` 持久化全局历史上限；通过
`clear(confirm=True)` 保留当前文件并永久清空历史。

`begin()` 和 `transaction()` 只为兼容保留，调用会明确报错，因为系统
已没有手工事务模式。
