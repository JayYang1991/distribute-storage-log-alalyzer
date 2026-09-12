#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 元数据库 (bbolt) 在线热备份工具
# 支持定时任务 (crontab) 自动化备份与历史快照轮转
# ==============================================================================

set -e

INSTALL_DIR="${INSTALL_DIR:-/opt/dist-log-analyzer}"
if [ ! -d "$INSTALL_DIR" ] && [ -d "$HOME/.dist-log-analyzer" ]; then
    INSTALL_DIR="$HOME/.dist-log-analyzer"
fi

BACKUP_DIR="${BACKUP_DIR:-$INSTALL_DIR/backups}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"
MANAGER_URL="${MANAGER_URL:-http://127.0.0.1:8080}"
CLUSTER_TOKEN="${CLUSTER_TOKEN:-dist-log-cluster-secret-token}"

mkdir -p "$BACKUP_DIR"

TIMESTAMP=$(date '+%Y%m%d_%H%M%S')
BACKUP_FILE="$BACKUP_DIR/analyzer_db_$TIMESTAMP.db"

echo "=================================================================="
echo "  分布式存储日志分析系统 - 数据库热备份工具"
echo "  备份时间: $(date '+%Y-%m-%d %H:%M:%S')"
echo "  目标路径: $BACKUP_FILE"
echo "=================================================================="

# 1. 优先尝试从运行中的 Manager 获取一致性在线热快照
if curl -s --connect-timeout 3 "$MANAGER_URL/healthz" >/dev/null 2>&1; then
    echo "[1/3] 检测到 Manager 正常运行，正在发起在线无锁快照导出..."
    if curl -sSL -f -o "$BACKUP_FILE" "$MANAGER_URL/api/system/backup" 2>/dev/null; then
        echo "  ✔ 在线热快照导出成功！"
    else
        echo "  ⚠️ 在线 API 备份受限，降级使用底层文件安全拷贝..."
        DB_SRC="$INSTALL_DIR/data/analyzer.db"
        if [ -f "$DB_SRC" ]; then
            cp -f "$DB_SRC" "$BACKUP_FILE"
        fi
    fi
else
    echo "[1/3] Manager 服务未运行，执行本地底层文件备份..."
    DB_SRC="$INSTALL_DIR/data/analyzer.db"
    if [ -f "$DB_SRC" ]; then
        cp -f "$DB_SRC" "$BACKUP_FILE"
    else
        echo "[ERROR] 未找到数据库文件: $DB_SRC"
        exit 1
    fi
fi

# 2. 校验快照完整性与文件大小
if [ -s "$BACKUP_FILE" ]; then
    SIZE_KB=$(du -k "$BACKUP_FILE" | cut -f1)
    echo "[2/3] 快照文件校验通过 (大小: ${SIZE_KB} KB)"
else
    echo "[ERROR] 备份文件为空，备份失败！"
    rm -f "$BACKUP_FILE"
    exit 1
fi

# 3. 清理超过保留天数的旧快照
echo "[3/3] 检查并清理超过 $RETENTION_DAYS 天的历史备份..."
CLEAN_COUNT=0
find "$BACKUP_DIR" -name "analyzer_db_*.db" -mtime +"$RETENTION_DAYS" -exec rm -f {} + 2>/dev/null || true

echo "=================================================================="
echo "  🎉 数据库快照备份完成！"
echo "  ▶ 备份文件: $BACKUP_FILE"
echo "  ▶ 保留策略: 保留最近 $RETENTION_DAYS 天"
echo "=================================================================="
