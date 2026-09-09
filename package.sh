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
cp -f "$PROJECT_ROOT/scripts/service.sh" "$STAGE_DIR/scripts/service.sh"
chmod +x "$STAGE_DIR/install.sh" "$STAGE_DIR/scripts/"*.sh

# 生成默认配置文件模版
cat > "$STAGE_DIR/conf/config.example.json" <<EOF
{
  "role": "manager",
  "port": 8080,
  "listen_host": "0.0.0.0",
  "data_dir": "./data",
  "cluster_token": "dist-log-cluster-secret-token",
  "jwt_secret": "dist-log-jwt-secret-key-2026",
  "initial_admin": {
    "username": "admin",
    "password": "admin123"
  }
}
EOF

# 生成安装包内的自述文档
cat > "$STAGE_DIR/README.md" <<EOF
# 分布式存储日志分析系统 (Distributed Storage Log Analyzer)

本系统采用纯静态单一二进制构建，零外部依赖，天然适配通用 RedHat / CentOS 7/8/9、Rocky Linux、AlmaLinux 等主流 Linux 环境。

## 快速一键安装管理组件 (Manager)
\`\`\`bash
# 默认安装并启动管理组件 (HTTP: 8080)
sudo ./install.sh

# 或者自定义端口与数据目录安装:
sudo ./install.sh --role=manager --port=8080 --data-dir=/opt/dist-log/data
\`\`\`

安装完成后，打开浏览器访问控制台：
- 访问地址: http://<服务器IP>:8080
- 默认管理员账号: admin
- 默认管理员密码: admin123

## 在 Web 控制台一键安装业务组件 (Worker)
1. 登录管理员控制台；
2. 点击侧边栏【集群节点与安装】；
3. 点击【一键安装业务组件 (SSH)】，填写目标业务主机 IP 及 SSH 凭据，系统将自动进行 SFTP 推包与一键远程部署；
4. 或者复制界面提供的离线安装命令在目标主机执行。

## 服务管理命令
- 查看状态: \`./scripts/service.sh status\` (或 \`systemctl status dist-log-manager\`)
- 停止服务: \`./scripts/service.sh stop\`   (或 \`systemctl stop dist-log-manager\`)
- 重启服务: \`./scripts/service.sh restart\`(或 \`systemctl restart dist-log-manager\`)
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
echo "     sudo ./install.sh"
echo "=================================================================="
