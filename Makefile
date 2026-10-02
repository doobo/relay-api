# Relay-Api - 构建与开发命令
#
# 前端通过 go:embed 内嵌，修改 web/ 后重新 make build 即可，无需额外构建步骤。

BINARY   ?= relay-api
PKG      := .
GO       ?= go
DIST     ?= dist
# 静态编译：SQLite 驱动为纯 Go 的 modernc.org/sqlite，无需 CGO。
export CGO_ENABLED ?= 0
# 版本号：优先取 git describe，没有 git 仓库或标签时回退到 dev。
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"
# 交叉编译的发布平台，可用 PLATFORMS="linux/amd64 linux/arm64" 覆盖。
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
# 覆盖运行配置，例如：make run ENV="PORT=8080 LOG_LEVEL=debug"
ENV ?=

.DEFAULT_GOAL := build

.PHONY: all build run test test-v race cover vet fmt fmt-check tidy clean distclean release clean-dist help

## all: 编译并运行测试（提交前请用这个）
all: build test

## build: 编译静态二进制到 ./$(BINARY)
build:
	$(GO) build $(BUILD_FLAGS) -o $(BINARY) $(PKG)

## run: 本地启动服务。用法：make run [ENV="PORT=8080 LOG_LEVEL=debug"]
run:
	$(ENV) $(GO) run $(PKG)

## test: 运行全部测试
test:
	$(GO) test ./...

## test-v: 以详细输出运行测试
test-v:
	$(GO) test -v ./...

## race: 带竞态检测运行测试
race:
	$(GO) test -race ./...

## cover: 生成覆盖率报告 coverage.html
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "coverage.html 已生成"

## vet: go vet 静态检查
vet:
	$(GO) vet ./...

## fmt: 格式化 Go 代码
fmt:
	$(GO) fmt ./...

## fmt-check: 检查格式是否有差异（CI 用，有差异时返回非 0）
fmt-check:
	@gofmt="$$($(GO) env GOROOT)/bin/gofmt"; \
	out="$$($$gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "以下文件需要 gofmt:"; echo "$$out"; exit 1; fi; \
	echo "gofmt 检查通过"

## tidy: 整理依赖（go.mod / go.sum）
tidy:
	$(GO) mod tidy

## release: 交叉编译各平台发布包到 $(DIST)/，并生成 SHA256SUMS
release:
	@$(MAKE) --no-print-directory clean-dist
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		name="$(BINARY)-$(VERSION)-$$os-$$arch"; \
		echo "==> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(BUILD_FLAGS) -o $(DIST)/$$name$$ext $(PKG) || exit 1; \
		if [ "$$os" = "windows" ] && command -v zip >/dev/null 2>&1; then \
			( cd $(DIST) && zip -q "$$name.zip" "$$name$$ext" ) || exit 1; \
			rm -f $(DIST)/$$name$$ext; \
		else \
			if [ "$$os" = "windows" ]; then echo "    未找到 zip，改用 tar.gz 打包"; fi; \
			tar -C $(DIST) -czf $(DIST)/$$name.tar.gz "$$name$$ext" || exit 1; \
			rm -f $(DIST)/$$name$$ext; \
		fi; \
	done
	@cd $(DIST) && files="$$(ls *.tar.gz *.zip 2>/dev/null)"; \
	if [ -z "$$files" ]; then echo "没有生成任何发布包"; exit 1; fi; \
	if command -v sha256sum >/dev/null 2>&1; then echo "$$files" | tr ' ' '\\n' | xargs sha256sum > SHA256SUMS; \
	else echo "$$files" | tr ' ' '\\n' | xargs shasum -a 256 > SHA256SUMS; fi
	@echo "发布包已生成于 $(DIST)/ (VERSION=$(VERSION)):"; ls -1 $(DIST)

## clean-dist: 删除 $(DIST) 发布目录
clean-dist:
	rm -rf $(DIST)

## clean: 删除编译产物与覆盖率文件
clean:
	rm -f $(BINARY) $(BINARY).exe coverage.out coverage.html

## distclean: clean + 清理 go build 缓存
distclean: clean
	$(GO) clean -cache -testcache

## help: 列出所有可用目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
