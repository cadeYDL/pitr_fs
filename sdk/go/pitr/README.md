# PITR-FS Go SDK

```go
client, err := pitr.Dial("/var/run/pitrd.sock")
if err != nil {
    return err
}
defer client.Close()

transaction, err := client.Begin(
    ctx,
    "/workspace/project",
    pitr.WithMessage("agent edit"),
)
if err != nil {
    return err
}

if err := os.WriteFile(
    "/workspace/project/result.txt",
    []byte("done\n"),
    0o644,
); err != nil {
    _ = transaction.Rollback(ctx)
    return err
}
return transaction.Commit(ctx, "write result")
```

SDK 只负责事务和时间旅行控制；文件操作继续使用标准 `os`/`io` API，
由 FUSE 自动生成版本。
