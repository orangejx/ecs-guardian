# ecs-guardian Makefile —— 本地快速安装环境 & 编译可执行文件
#
# 常用命令：
#   make deps        安装/更新 Go 依赖（go mod tidy）
#   make build       编译当前平台的可执行文件（输出 bin/ecs-guardian）
#   make build-all   交叉编译 linux/amd64 + linux/arm64（输出 bin/）
#   make version     编译并打印版本号
#   make validate    编译后用 config.example.json 校验配置
#   make run         编译后直接以常驻方式运行（默认读 /data/config.json）
#   make test        编译 + go vet + 校验配置样例
#   make clean       清理编译产物
#
# 版本号：默认取最近一次 git tag（去掉 v 前缀），可覆盖：make build VERSION=0.0.2

SHELL := /bin/sh
BINARY := ecs-guardian
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all deps build build-all version validate run test clean

all: build

## 安装/更新 Go 依赖（首次拉取本仓库后先执行）
deps:
	go mod tidy
	go mod download

## 编译当前平台可执行文件
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) .
	@echo "已生成 $(BIN_DIR)/$(BINARY) (version=$(VERSION))"

## 交叉编译 linux/amd64 + linux/arm64 双架构
build-all:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-arm64 .
	@echo "已生成 linux/amd64 与 linux/arm64 可执行文件 (version=$(VERSION))"

## 编译并打印版本
version: build
	./$(BIN_DIR)/$(BINARY) version

## 编译并用 config.example.json 校验配置
validate: build
	./$(BIN_DIR)/$(BINARY) validate --config config.example.json

## 编译后以常驻方式运行（可用 CONFIG 指定配置文件：make run CONFIG=/path/to/config.json）
run: build
	./$(BIN_DIR)/$(BINARY) $(if $(CONFIG),--config $(CONFIG),)

## 基础检查：编译 + vet + 校验配置样例
test: build
	go vet ./...
	./$(BIN_DIR)/$(BINARY) validate --config config.example.json
	@echo "全部检查通过 ✅"

## 清理编译产物
clean:
	rm -rf $(BIN_DIR)
