#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 元数据库 (bbolt) 灾难恢复工具
# ==============================================================================

set -e

INSTALL_DIR="${INSTALL_DIR:-/opt/dist-log-analyzer}"
if [ ! -d "$INSTALL_DIR" ] && [ -d "$HOME/.dist-log-analyzer" ]; then
    INSTALL_DIR="$HOME/.dist-log-analyzer"
fi

DATA_DIR="$INSTALL_DIR/data"
TARGET_DB="$DATA_DIR/analyzer.db"

SNAPSHOT_FILE="$1"
if [ -z "$SNAPSHOT_FILE" ]; then
    echo "使用方法: $0 <快照文件路径.db> [--yes]"
    echo ""
    echo "最近可用的备份快照列表:"
    find "$INSTALL_DIR/backups" -name "analyzer_db_*.db" 2>/dev/null | sort -r | head -n 5 || echo "  (无历史备份)"
    exit 1
fi

if [ ! -f "$SNAPSHOT_FILE" ]; then
    echo "[ERROR] 快照文件不存在: $SNAPSHOT_FILE"
    exit 1
fi

AUTO_CONFIRM=false
if [ "$2" == "--yes" ] || [ "$2" == "-y" ]; then
    AUTO_CONFIRM=true
fi

echo "=================================================================="
echo "  ⚠️ 警告: 数据库灾难恢复即将开始！"
echo "  恢复来源: $SNAPSHOT_FILE"
echo "  恢复目标: $TARGET_DB"
echo "=================================================================="

if [ "$AUTO_CONFIRM" = false ]; then
    read -p "确定要停止 Manager 服务并恢复此快照吗？(y/N): " CONFIRM
    if [[ ! "$CONFIRM" =~ ^[yY]$ ]]; then
        echo "已取消恢复操作。"
        exit 0
    fi
fi

# 1. 停止 Manager 服务
echo "[1/4] 停止 Manager 运行服务..."
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet dist-log-manager 2>/dev/null; then
    systemctl stop dist-log-manager || true
else
    pkill -f "$INSTALL_DIR/bin/dist-log-analyzer manager" 2>/dev/null || true
fi
sleep 1

# 2. 备份当前现有数据库
echo "[2/4] 备份当前运行数据至 $TARGET_DB.bak_$(date '+%Y%m%d_%H%M%S')..."
if [ -f "$TARGET_DB" ]; then
    cp -f "$TARGET_DB" "$TARGET_DB.bak_$(date '+%Y%m%d_%H%M%S')"
fi

# 3. 替换快照文件并确保权限
echo "[3/4] 恢复快照并校准权限..."
cp -f "$SNAPSHOT_FILE" "$TARGET_DB"
chmod 600 "$TARGET_DB"

# 4. 重启 Manager 服务并执行健康验证
echo "[4/4] 重新启动 Manager 服务..."
if command -v systemctl >/dev/null 2>&1 && [ -f "/etc/systemd/system/dist-log-manager.service" ]; then
    systemctl restart dist-log-manager
    sleep 2
    if systemctl is-active --quiet dist-log-manager; then
        echo "  ✔ Systemd 服务恢复成功并通过健康自检！"
    else
        echo "  ❌ 服务重启异常，请检查日志！"
        systemctl status dist-log-manager --no-pager || true
        exit 1
    fi
else
    echo "  请使用 $INSTALL_DIR/service.sh manager start 启动服务"
fi

echo "=================================================================="
echo "  🎉 数据库快照已成功恢复完成！"
echo "=================================================================="
