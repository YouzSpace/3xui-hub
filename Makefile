# ControlHub 构建与校验入口 / Build & verify entry point
#
# 用法 / Usage:  make help
#
# 注意 / Note: help 里的文字用 make 的 $(info) 直接输出，不走 shell ——
# Windows 下 recipe 默认走 cmd.exe，中文 echo 会被按 GBK 打成乱码。
# help text is printed with $(info) instead of echo so UTF-8 stays intact on Windows.

GO       ?= go
NPM      ?= npm
GO_TAGS  ?= embed
SERVER   := server
FRONTEND := frontend

.DEFAULT_GOAL := help
.PHONY: help deps build build-backend build-frontend test test-backend test-frontend typecheck lint verify

help: ## 列出所有可用目标 / List all available targets
	$(info ControlHub —— 构建与校验 / build & verify)
	$(info )
	$(info   make build            构建前端 + 编译后端 / build frontend, then compile backend)
	$(info   make build-frontend   只构建前端，产物进 server/internal/web/dist / frontend only)
	$(info   make build-backend    只编译后端，带 -tags=$(GO_TAGS) / backend only)
	$(info   make test             后端单测 + 前端单测 / backend tests + frontend tests)
	$(info   make test-backend     只跑后端单测 go test ./... / backend tests only)
	$(info   make test-frontend    只跑前端单测 vitest / frontend tests only)
	$(info   make typecheck        前端类型检查，无 typecheck 脚本时自动跳过 / skip when absent)
	$(info   make lint             后端 golangci-lint，需本机已安装 / requires golangci-lint)
	$(info   make verify           build + test + typecheck，提交前跑这个 / run before pushing)
	$(info   make deps             装前端依赖，node_modules 缺失时才执行 / npm ci when needed)
	$(info )
	$(info 变量 / variables:  GO=$(GO)  NPM=$(NPM)  GO_TAGS=$(GO_TAGS))
	@cd .

# 前端依赖：node_modules 不存在时自动 npm ci，已装则整行跳过（make 在解析期判断）。
# Install frontend deps only when missing; a no-op for anyone who already ran npm ci.
deps:
ifeq ($(wildcard $(FRONTEND)/node_modules),)
	cd $(FRONTEND) && $(NPM) ci
endif

# 顺序不能反：前端产物是后端 -tags=embed 的输入，先有 dist 才能编出单文件二进制。
# Order matters: the SPA build output is the input of the embed build.
build: build-frontend build-backend build-agent ## 前端 + 后端 + agent 全量构建 / full build

build-frontend: deps ## 构建前端 / build frontend
	cd $(FRONTEND) && $(NPM) run build

build-backend: ## 编译后端全部包 / compile backend packages
	cd $(SERVER) && $(GO) build -tags=$(GO_TAGS) ./...

build-agent: ## 编译 agent（独立 module，别漏）/ compile agent (separate module)
	cd agent && $(GO) build ./...

test: test-backend test-agent test-frontend ## 后端 + agent + 前端全部单测 / all tests

test-backend: ## 后端单测 / backend tests
	cd $(SERVER) && $(GO) test ./...

test-agent: ## agent 单测（独立 module，别漏）/ agent tests (separate module)
	cd agent && $(GO) test ./...

test-frontend: deps ## 前端单测 / frontend tests
	cd $(FRONTEND) && $(NPM) test

typecheck: deps ## 前端类型检查（无脚本时跳过）/ frontend typecheck when present
	cd $(FRONTEND) && $(NPM) run typecheck --if-present

# golangci-lint 配置在 server/.golangci.yml。存量代码仍有告警（unused / errcheck 等），
# 清理完之前本目标会红——它只反映代码现状，不阻塞 build / test。
lint: ## 后端 golangci-lint / lint backend
	cd $(SERVER) && golangci-lint run ./...

verify: build test typecheck ## 提交前校验：构建 + 单测 + 类型检查 / pre-push check
	$(info )
	$(info verify 全部通过 / all checks passed)
