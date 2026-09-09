#!/usr/bin/env bash
# 分布式存储日志分析系统 服务控制脚本 (适用于任意 Linux 发行版，无 systemd 亦可运行)

if [ -f "$(dirname "$0")/bin/dist-log-analyzer" ]; then
    BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
else
    BASE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
fi
BIN="$BASE_DIR/bin/dist-log-analyzer"
PID_FILE="$BASE_DIR/run/service.pid"
LOG_FILE="$BASE_DIR/logs/service.log"
CONF_FILE="$BASE_DIR/conf/config.json"

mkdir -p "$BASE_DIR/run" "$BASE_DIR/logs" "$BASE_DIR/data"

ROLE="${1:-manager}"
ACTION="${2:-status}"

# 如果第一个参数是 start/stop/restart/status
if [[ "$ROLE" =~ ^(start|stop|restart|status)$ ]]; then
    ACTION="$ROLE"
    ROLE="manager"
fi

get_pid() {
    if [ -f "$PID_FILE" ]; then
        cat "$PID_FILE"
    else
        pgrep -f "$BIN $ROLE" | head -n 1
    fi
}

start_service() {
    PID=$(get_pid)
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        echo "[INFO] 服务已经在运行中 (PID: $PID)"
        return 0
    fi

    echo "[INFO] 正在启动分布式日志分析系统 [$ROLE 组件]..."
    if [ "$ROLE" == "manager" ]; then
        nohup "$BIN" manager --data-dir="$BASE_DIR/data" </dev/null >> "$LOG_FILE" 2>&1 &
    else
        nohup "$BIN" worker --data-dir="$BASE_DIR/data" </dev/null >> "$LOG_FILE" 2>&1 &
    fi

    NEW_PID=$!
    disown $NEW_PID 2>/dev/null || true
    echo "$NEW_PID" > "$PID_FILE"
    sleep 1

    if kill -0 "$NEW_PID" 2>/dev/null; then
        echo "[OK] 服务启动成功！(PID: $NEW_PID)"
        echo "[OK] 查看日志: $LOG_FILE"
    else
        echo "[ERROR] 服务启动失败，请检查日志: $LOG_FILE"
        exit 1
    fi
}

stop_service() {
    PID=$(get_pid)
    if [ -z "$PID" ] || ! kill -0 "$PID" 2>/dev/null; then
        echo "[INFO] 服务未在运行"
        rm -f "$PID_FILE"
        return 0
    fi

    echo "[INFO] 正在停止服务 (PID: $PID)..."
    kill "$PID" 2>/dev/null || true
    
    # 等待优雅停止
    for i in {1..10}; do
        if ! kill -0 "$PID" 2>/dev/null; then
            break
        fi
        sleep 0.5
    done

    if kill -0 "$PID" 2>/dev/null; then
        echo "[WARN] 正在强制停止..."
        kill -9 "$PID" 2>/dev/null || true
    fi

    rm -f "$PID_FILE"
    echo "[OK] 服务已停止"
}

status_service() {
    PID=$(get_pid)
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        echo "[OK] 服务正常运行中 (PID: $PID, Role: $ROLE)"
    else
        echo "[INFO] 服务当前处于停止状态"
    fi
}

case "$ACTION" in
    start)
        start_service
        ;;
    stop)
        stop_service
        ;;
    restart)
        stop_service
        start_service
        ;;
    status)
        status_service
        ;;
    *)
        echo "使用方法: $0 [manager|worker] {start|stop|restart|status}"
        exit 1
        ;;
esac
