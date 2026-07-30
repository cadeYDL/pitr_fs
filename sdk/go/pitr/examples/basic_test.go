package examples

import (
	"context"
	"os"

	"pitr_fs/sdk/go/pitr"
)

// Example 展示 SDK 与标准 os API 的组合。设置 PITR_EXAMPLE_SOCKET 和
// PITR_EXAMPLE_PATH 后可作为真实端到端 example 执行。
func Example() {
	socket := os.Getenv("PITR_EXAMPLE_SOCKET")
	path := os.Getenv("PITR_EXAMPLE_PATH")
	if socket == "" || path == "" {
		return
	}
	client, err := pitr.Dial(socket)
	if err != nil {
		panic(err)
	}
	defer client.Close()
	if err := os.WriteFile(path+"/hello.txt", []byte("hello\n"), 0o644); err != nil {
		panic(err)
	}
	if _, err := client.Logs(context.Background(), path, 20); err != nil {
		panic(err)
	}
}
