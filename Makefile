# ZTAG 常用开发命令
BINARY := bin/ztag

.PHONY: build run tidy vet clean

## 编译服务
build:
	go build -o $(BINARY) ./cmd/server

## 直接运行（首次启动会自动生成 data/config.toml）
run:
	go run ./cmd/server

## 整理依赖
tidy:
	go mod tidy

## 静态检查
vet:
	go vet ./...

## 清理构建产物
clean:
	rm -rf bin