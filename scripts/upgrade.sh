#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 一键平滑滚动升级工具 (带健康检查与自动回滚)
# ==============================================================================

set -e

INSTALL_DIR="${INSTALL_DIR:-/opt/dist-log-analyzer}"
if [ ! -d "$INSTALL_DIR" ] && [ -d "$HOME/.dist-log-analyzer" ]; then
    INSTALL_DIR="$HOME/.dist-log-analyzer"
fi

BIN="$INSTALL_DIR/bin/dist-log-analyzer"
BIN_BAK="$INSTALL_DIR/bin/dist-log-analyzer.bak"
NEW_BIN="$1"

if [ -z "$NEW_BIN" ]; then
    echo "使用方法: $0 <新版本二进制路径 dist-log-analyzer>"
    echo "示例: sudo $0 /tmp/dist-log-analyzer-new"
    exit 1
fi

if [ ! -f "$NEW_BIN" ]; then
    echo "[ERROR] 未找到新版本二进制文件: $NEW_BIN"
    exit 1
fi

echo "=================================================================="
echo "  分布式存储日志分析系统 - 平滑版本升级"
echo "  当前程序: $BIN"
echo "  目标程序: $NEW_BIN"
echo "=================================================================="

# 1. 验证新二进制是否具备执行能力并输出版本
echo "[1/4] 验证新版本二进制合法性与架构兼容性..."
chmod +x "$NEW_BIN"
if ! "$NEW_BIN" --help >/dev/null 2>&1; then
    echo "[ERROR] 新版本二进制执行测试失败（可能架构不兼容或二进制损坏）！中止升级。"
    exit 1
fi

# 2. 备份当前旧版本
echo "[2/4] 备份当前运行版本到 $BIN_BAK..."
if [ -f "$BIN" ]; then
    cp -f "$BIN" "$BIN_BAK"
fi

# 3. 替换二进制
echo "[3/4] 替换为新版本程序..."
cp -f "$NEW_BIN" "$BIN"
chmod +x "$BIN"

# 4. 重启关联的全部 systemd 服务并执行存活健康检查
echo "[4/4] 滚动重启受影响的服务并执行健康自检..."
SERVICES_TO_CHECK=()

if command -v systemctl >/dev/null 2>&1; then
    for s in $(systemctl list-units --type=service --state=running --no-pager | grep -E "dist-log-manager|dist-log-worker" | awk '{print $1}'); do
        echo "  ▶ 正在平滑重启服务: $s..."
        systemctl restart "$s"
        SERVICES_TO_CHECK+=("$s")
    done
fi

# 等待 2 秒检测存活
sleep 2

ALL_HEALTHY=true
for s in "${SERVICES_TO_CHECK[@]}"; do
    if systemctl is-active --quiet "$s"; then
        echo "  ✔ 服务 $s 健康自检通过 (active)！"
    else
        echo "  ❌ 服务 $s 重启后未处于 active 状态！"
        ALL_HEALTHY=false
    fi
done

if [ "$ALL_HEALTHY" = false ]; then
    echo ""
    echo "=================================================================="
    echo "  🚨 升级健康自检失败！触发自动秒级回滚！"
    echo "=================================================================="
    if [ -f "$BIN_BAK" ]; then
        cp -f "$BIN_BAK" "$BIN"
        for s in "${SERVICES_TO_CHECK[@]}"; do
            echo "  ▶ 正在回滚重启服务: $s..."
            systemctl restart "$s" || true
        done
        echo "  ✔ 已自动安全回滚至上一稳定版本！"
    fi
    exit 1
fi

echo ""
echo "=================================================================="
echo "  🎉 系统升级成功！所有服务运行正常并已生效最新版本！"
echo "  ▶ 备份版本保留在: $BIN_BAK"
echo "  ▶ 若后续出现异常，可随时执行 ./rollback.sh 一键原路回滚"
echo "=================================================================="
