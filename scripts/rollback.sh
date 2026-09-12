#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 一键快速回滚工具
# ==============================================================================

set -e

INSTALL_DIR="${INSTALL_DIR:-/opt/dist-log-analyzer}"
if [ ! -d "$INSTALL_DIR" ] && [ -d "$HOME/.dist-log-analyzer" ]; then
    INSTALL_DIR="$HOME/.dist-log-analyzer"
fi

BIN="$INSTALL_DIR/bin/dist-log-analyzer"
BIN_BAK="$INSTALL_DIR/bin/dist-log-analyzer.bak"

if [ ! -f "$BIN_BAK" ]; then
    echo "[ERROR] 未找到可回滚的历史备份版本: $BIN_BAK"
    exit 1
fi

echo "=================================================================="
echo "  分布式存储日志分析系统 - 一键回滚上一版本"
echo "  当前程序: $BIN"
echo "  回滚来源: $BIN_BAK"
echo "=================================================================="

# 1. 回滚替换二进制
echo "[1/2] 还原二进制程序..."
cp -f "$BIN_BAK" "$BIN"
chmod +x "$BIN"

# 2. 重启服务并检查健康
echo "[2/2] 重新启动所有服务..."
if command -v systemctl >/dev/null 2>&1; then
    for s in $(systemctl list-units --type=service --all --no-pager | grep -E "dist-log-manager|dist-log-worker" | awk '{print $1}'); do
        echo "  ▶ 正在重启服务: $s..."
        systemctl restart "$s"
        sleep 1
        if systemctl is-active --quiet "$s"; then
            echo "  ✔ 服务 $s 状态正常！"
        else
            echo "  ⚠️ 服务 $s 状态异常"
        fi
    done
fi

echo "=================================================================="
echo "  🎉 系统已成功回滚至上一稳定版本！"
echo "=================================================================="
