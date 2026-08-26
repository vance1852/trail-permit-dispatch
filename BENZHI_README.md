# BENZHI_README

这是一个 Go 后端服务，面向户外徒步（驴友）组织方的后端系统：以线路每日许可配额为核心资源，治理「计划 → 队员登记 → 许可占用 → 复核放行 → 在途打点 → 事故处置 → 许可结算 → 审计」的完整生命周期。

## 标准构建、运行和测试命令

进入容器后执行：

```bash
# 编译
cd '/app' && GOTOOLCHAIN=local go build ./...

# 启动
cd '/app' && GOTOOLCHAIN=local go run ./cmd/server

# 测试
cd '/app' && GOTOOLCHAIN=local go test ./...
```

## Docker 构建和进入容器

```bash
chmod +x build_benzhi_docker.sh
./build_benzhi_docker.sh benzhi-task-366-amd64 linux/amd64
./build_benzhi_docker.sh benzhi-task-366-arm64 linux/arm64
docker run -it benzhi-task-366-amd64:latest
docker run -it --platform linux/arm64 benzhi-task-366-arm64:latest
```
