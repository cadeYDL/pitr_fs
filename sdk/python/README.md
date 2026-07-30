# PITR-FS Python SDK

```python
from pitr import Client

with Client("/var/run/pitrd.sock") as client:
    with client.transaction("/workspace/project", "agent edit"):
        with open("/workspace/project/result.txt", "w") as file:
            file.write("done\n")
```

上下文正常退出时自动 commit；抛出异常时自动 rollback。
