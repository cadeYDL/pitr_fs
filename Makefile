# 所有开发/测试都在 orbstack calw 内进行 (先: orb shell calw).

.PHONY: test

test:
	@if [ -z "$$(go list ./... 2>/dev/null)" ]; then \
		echo "(尚无 Go 包, 跳过)"; \
	else \
		go test ./...; \
	fi
