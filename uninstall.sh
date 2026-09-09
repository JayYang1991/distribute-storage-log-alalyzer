#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 (Distributed Storage Log Analyzer)
# 一键卸载与环境清理脚本
# ==============================================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# 默认参数
INSTALL_DIR="/opt/dist-log-analyzer"
REMOVE_DATA=false
FORCE=false
ROLE=""

# 解析命令行参数
while [[ $# -gt 0 ]]; do
    case "$1" in
        --install-dir=*)
            INSTALL_DIR="${1#*=}"
            ;;
        --role=*)
            ROLE="${1#*=}"
            ;;
        --remove-data|--purge|-f)
            REMOVE_DATA=true
            ;;
        --force|-y)
            FORCE=true
            ;;
        -h|--help)
            echo "分布式存储日志分析系统 一键卸载工具"
            echo "使用方法: $0 [选项]"
            echo ""
            echo "选项说明:"
            echo "  --install-dir=<目录>   指定程序安装路径 (默认: /opt/dist-log-analyzer)"
            echo "  --role=manager|worker  仅卸载指定角色组件 (默认两者均检测清理)"
            echo "  --purge, --remove-data 彻底清理所有数据目录 (包括用户日志、SQLite 数据库、解压文件)"
            echo "  --force, -y            免交互静默强制卸载"
            echo "  -h, --help             显示此帮助信息"
            echo ""
            echo "示例:"
            echo "  sudo ./uninstall.sh                           # 安全卸载程序，默认保留数据"
            echo "  sudo ./uninstall.sh --purge -y                # 静默彻底卸载，包括全部历史数据"
            echo "  sudo ./uninstall.sh --install-dir=/opt/dist-log-analyzer-manager"
            exit 0
            ;;
        *)
            echo "[WARN] 忽略未知参数: $1"
            ;;
    esac
    shift
done

echo "=================================================================="
echo "    分布式存储日志分析系统 (Distributed Storage Log Analyzer)     "
echo "    一键快速卸载程序                                              "
echo "=================================================================="
echo "  ▶ 目标安装路径: $INSTALL_DIR"
echo "  ▶ 彻底清理数据: $([ "$REMOVE_DATA" = true ] && echo "是 (数据将被删除)" || echo "否 (保留数据目录)")"
echo "=================================================================="

# 1. 检查 root 权限
IS_ROOT=false
if [ "$(id -u)" -eq 0 ]; then
    IS_ROOT=true
else
    echo "[WARN] 当前非 root 用户，如果服务以 root/systemd 注册，请使用 sudo ./uninstall.sh 执行"
fi

# 2. 交互确认
if [ "$FORCE" != true ]; then
    read -r -p "确认要停止服务并卸载系统吗？[y/N] " confirm
    if [[ ! "$confirm" =~ ^[yY]([eE][sS])?$ ]]; then
        echo "已取消卸载。"
        exit 0
    fi

    if [ "$REMOVE_DATA" != true ] && [ -d "$INSTALL_DIR/data" ]; then
        read -r -p "是否同时删除数据目录 ($INSTALL_DIR/data) 及历史日志？[y/N] " del_data
        if [[ "$del_data" =~ ^[yY]([eE][sS])?$ ]]; then
            REMOVE_DATA=true
        fi
    fi
fi

# 3. 停止并注销 Systemd 守护服务
echo "[1/4] 检查并停止运行中的服务..."
SERVICES=("dist-log-manager.service" "dist-log-worker.service")

if [ -n "$ROLE" ]; then
    SERVICES=("dist-log-$ROLE.service")
fi

if [ "$IS_ROOT" = true ] && command -v systemctl >/dev/null 2>&1; then
    for svc in "${SERVICES[@]}"; do
        if systemctl list-units --type=service --all | grep -q "$svc" || [ -f "/etc/systemd/system/$svc" ]; then
            echo "  ▶ 正在停止 Systemd 服务: $svc..."
            systemctl stop "$svc" 2>/dev/null || true
            systemctl disable "$svc" 2>/dev/null || true
            if [ -f "/etc/systemd/system/$svc" ]; then
                rm -f "/etc/systemd/system/$svc"
                echo "  ▶ 已注销服务配置: /etc/systemd/system/$svc"
            fi
        fi
    done
    systemctl daemon-reload 2>/dev/null || true
    systemctl reset-failed 2>/dev/null || true
fi

# 4. 检查并终止可能的残留进程
echo "[2/4] 检查并终止后台运行的孤儿进程..."
PIDS=$(pgrep -f "dist-log-analyzer" 2>/dev/null || true)
if [ -n "$PIDS" ]; then
    echo "  ▶ 发现运行中的 dist-log-analyzer 进程，正在退出..."
    for p in $PIDS; do
        kill "$p" 2>/dev/null || true
    done
    sleep 1
    # 若仍在运行则强制 SIGKILL
    REMAIN_PIDS=$(pgrep -f "dist-log-analyzer" 2>/dev/null || true)
    if [ -n "$REMAIN_PIDS" ]; then
        for rp in $REMAIN_PIDS; do
            kill -9 "$rp" 2>/dev/null || true
        done
    fi
fi

# 5. 清理程序文件与二进制
echo "[3/4] 清理安装程序文件与依赖..."
if [ -d "$INSTALL_DIR" ]; then
    if [ "$REMOVE_DATA" = true ]; then
        echo "  ▶ 正在彻底删除安装目录: $INSTALL_DIR (包含数据与日志)..."
        rm -rf "$INSTALL_DIR"
    else
        echo "  ▶ 正在清理程序二进制与脚本 (保留 $INSTALL_DIR/data)..."
        rm -rf "$INSTALL_DIR/bin" "$INSTALL_DIR/scripts" "$INSTALL_DIR/conf" "$INSTALL_DIR/rules" "$INSTALL_DIR/run" "$INSTALL_DIR/service.sh" "$INSTALL_DIR/install.sh"
        # 保留 data 目录，若目录已空则保留父级
        echo "  ✔ 业务数据已安全保留在: $INSTALL_DIR/data"
    fi
else
    echo "  ℹ 未检测到目录 $INSTALL_DIR，跳过文件清理"
fi

# 6. 检查自动检测到的其它常见路径 (例如 /opt/dist-log-analyzer-manager 或 worker)
EXTRA_DIRS=("/opt/dist-log-analyzer-manager" "/opt/dist-log-analyzer-worker1")
for extra in "${EXTRA_DIRS[@]}"; do
    if [ "$INSTALL_DIR" != "$extra" ] && [ -d "$extra" ] && [ "$FORCE" = true ]; then
        echo "  ▶ 连带清理测试实例目录: $extra..."
        rm -rf "$extra"
    fi
done

echo "[4/4] 验证卸载状态..."
if pgrep -f "dist-log-analyzer" >/dev/null 2>&1; then
    echo "  [WARN] 仍有组件进程在运行，请执行 ps aux | grep dist-log-analyzer 排查"
else
    echo "  ✔ 相关组件进程已全部停止退出"
fi

echo "=================================================================="
echo "          🎉 分布式存储日志分析系统已成功卸载！                   "
echo "=================================================================="
