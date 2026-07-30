# PITR-FS Go SDK

```go
client, err := pitr.Dial("/var/run/pitrd.sock")
if err != nil {
    return err
}
defer client.Close()

if err := os.WriteFile(
    "/workspace/project/result.txt",
    []byte("done\n"),
    0o644,
); err != nil {
    return err
}

// 文件关闭后已经自动形成版本。
versions, err := client.Logs(ctx, "/workspace/project", 20)
if err != nil {
    return err
}
_, err = client.Revert(
    ctx,
    versions[0].VersionHash,
    pitr.WithPath("/workspace/project"),
)
return err
```

文件操作继续使用标准 `os`/`io` API，由 FUSE 自动生成版本。`Revert`
默认当前目录，也可传 `WithGlobal()` 显式回退整个卷。

通过 `SetHistoryLimit(ctx, 100)` 持久化全局历史上限；通过
`Clear(ctx, true)` 在保留当前文件的前提下永久清空历史。

`Begin`/`Txn` 只为源代码兼容保留，调用会返回
`ErrManualTransactionsDisabled`。
