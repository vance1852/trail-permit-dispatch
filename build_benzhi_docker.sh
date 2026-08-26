#!/bin/bash
# 确保脚本在任何命令失败时立即退出
set -e

# 第一个参数作为镜像名，若未提供，则默认为 "my-project"
IMAGE_NAME=${1:-my-project}
# 第二个参数作为目标平台，若未提供，则默认为 "linux/amd64"
DOCKER_PLATFORM=${2:-linux/amd64}

# 使用评测专用的 benzhi.Dockerfile 构建（避开项目自带的 Dockerfile）
docker build --platform $DOCKER_PLATFORM -f benzhi.Dockerfile -t $IMAGE_NAME .

# 打印成功信息和下一步操作指引
echo ""
echo "✅ Docker image '$IMAGE_NAME' built successfully!"
echo ""
echo "📋 Next steps (for testing):"
echo "  • Interactive shell：docker run -it $IMAGE_NAME:latest"
echo ""
