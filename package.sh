#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 一键打包生成安装包脚本
# 目标架构: Linux x86_64 (amd64) 纯静态单二进制 (CGO_ENABLED=0)
# 特性: 零外部依赖，内嵌 Web 前端资产，可在 RedHat/CentOS 7/8/9、Rocky Linux、AlmaLinux 直接安装
# ==============================================================================

set -e

PROJECT_ROOT="$(cd "$(dirname "$0")" && pwd)"
PACKAGE_NAME="dist-log-analyzer-linux-amd64"
DIST_DIR="$PROJECT_ROOT/dist"
STAGE_DIR="$DIST_DIR/$PACKAGE_NAME"

echo "=================================================================="
echo "    分布式存储日志分析系统 - 一键生成部署安装包                    "
echo "=================================================================="

# 1. 查找 Go 编译器
if command -v go >/dev/null 2>&1; then
    GO_CMD="go"
elif [ -f "/home/jason/sdk/go/bin/go" ]; then
    GO_CMD="/home/jason/sdk/go/bin/go"
else
    echo "[ERROR] 未检测到 Go 编译环境！"
    exit 1
fi

# 自动解析构建版本号 (优先读取环境变量 GITHUB_REF_NAME 或 git describe，默认 1.0.0)
BUILD_VERSION="${GITHUB_REF_NAME:-}"
if [ -z "$BUILD_VERSION" ]; then
    BUILD_VERSION=$(git describe --tags --always 2>/dev/null || echo "1.0.0")
fi
BUILD_VERSION="${BUILD_VERSION#v}" # 去掉前导 v

echo "[1/4] 编译纯静态可执行二进制 (版本: $BUILD_VERSION, CGO_ENABLED=0, GOOS=linux, GOARCH=amd64)..."
mkdir -p "$PROJECT_ROOT/bin"
cd "$PROJECT_ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $GO_CMD build -ldflags="-s -w -X 'main.Version=$BUILD_VERSION'" -o "$PROJECT_ROOT/bin/dist-log-analyzer" ./cmd/analyzer

# 校验编译产物
if ! file "$PROJECT_ROOT/bin/dist-log-analyzer" | grep -q "statically linked"; then
    echo "[WARN] 编译产物可能并非静态链接，请核对 CGO_ENABLED=0 环境变量！"
fi

echo "[2/4] 构造安装包发布目录结构..."
rm -rf "$DIST_DIR"
mkdir -p "$STAGE_DIR/bin" "$STAGE_DIR/scripts" "$STAGE_DIR/conf" "$STAGE_DIR/rules" "$STAGE_DIR/logs" "$STAGE_DIR/run" "$STAGE_DIR/data"

# 复制核心二进制
cp -f "$PROJECT_ROOT/bin/dist-log-analyzer" "$STAGE_DIR/bin/dist-log-analyzer"
chmod +x "$STAGE_DIR/bin/dist-log-analyzer"

# 复制脚本
cp -f "$PROJECT_ROOT/scripts/install.sh" "$STAGE_DIR/install.sh"
cp -f "$PROJECT_ROOT/scripts/install.sh" "$STAGE_DIR/scripts/install.sh"
cp -f "$PROJECT_ROOT/scripts/uninstall.sh" "$STAGE_DIR/uninstall.sh"
cp -f "$PROJECT_ROOT/scripts/service.sh" "$STAGE_DIR/scripts/service.sh"
cp -f "$PROJECT_ROOT/scripts/backup.sh" "$STAGE_DIR/scripts/backup.sh"
cp -f "$PROJECT_ROOT/scripts/restore.sh" "$STAGE_DIR/scripts/restore.sh"
cp -f "$PROJECT_ROOT/scripts/upgrade.sh" "$STAGE_DIR/scripts/upgrade.sh"
cp -f "$PROJECT_ROOT/scripts/rollback.sh" "$STAGE_DIR/scripts/rollback.sh"
chmod +x "$STAGE_DIR/install.sh" "$STAGE_DIR/uninstall.sh" "$STAGE_DIR/scripts/"*.sh

# 生成默认配置文件模版
cat > "$STAGE_DIR/conf/config.example.json" <<EOF
{
  "role": "manager",
  "port": 8080,
  "listen_host": "0.0.0.0",
  "data_dir": "./data",
  "log_dir": "./logs",
  "log_level": "info",
  "cluster_token": "dist-log-cluster-secret-token"
}
EOF

# 生成包内快速指引
cat > "$STAGE_DIR/README.md" <<EOF
# 分布式存储日志分析系统 (Distributed Storage Log Analyzer)

## 快速安装
\`\`\`bash
# 1. 安装管理节点 Manager (默认端口 8080)
sudo ./install.sh --role=manager

# 2. 安装业务节点 Worker (默认端口 8081)
sudo ./install.sh --role=worker --manager-url=http://<管理节点IP>:8080
\`\`\`

## 服务管理命令
- 查看状态: \`./scripts/service.sh status\` (或 \`systemctl status dist-log-manager\`)
- 停止服务: \`./scripts/service.sh stop\`   (或 \`systemctl stop dist-log-manager\`)
- 重启服务: \`./scripts/service.sh restart\`(或 \`systemctl restart dist-log-manager\`)

## 一键卸载
- 安全卸载 (保留历史数据): \`sudo ./uninstall.sh\`
- 彻底卸载 (清除全部数据): \`sudo ./uninstall.sh --purge -y\`
EOF

echo "[3/4] 打包生成归档压缩包 ($PACKAGE_NAME.tar.gz)..."
cd "$DIST_DIR"
tar -zcvf "$PACKAGE_NAME.tar.gz" "$PACKAGE_NAME" >/dev/null

echo "[4/4] 计算 SHA256 校验和..."
sha256sum "$PACKAGE_NAME.tar.gz" > "$PACKAGE_NAME.tar.gz.sha256"

ARCHIVE_PATH="$DIST_DIR/$PACKAGE_NAME.tar.gz"
ARCHIVE_SIZE=$(du -h "$ARCHIVE_PATH" | awk '{print $1}')

echo ""
echo "=================================================================="
echo "    🎉 安装包打包成功！                                           "
echo "=================================================================="
echo "  ▶ 安装包文件: $ARCHIVE_PATH ($ARCHIVE_SIZE)"
echo "  ▶ 校验和文件: $DIST_DIR/$PACKAGE_NAME.tar.gz.sha256"
echo ""
echo "  ▶ 交付使用方法 (在目标机器上直接执行):"
echo "     tar -zxvf $PACKAGE_NAME.tar.gz"
echo "     cd $PACKAGE_NAME"
echo "     sudo ./install.sh       # 一键安装"
echo "     sudo ./uninstall.sh     # 一键卸载"
echo "=================================================================="
