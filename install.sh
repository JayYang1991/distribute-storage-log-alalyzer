#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 (Distributed Storage Log Analyzer)
# 一键安装脚本 (原生适配所有 RedHat / CentOS 衍生 Linux 及主流发行版，零额外依赖)
# ==============================================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CURRENT_DIR="$(pwd)"

# 默认配置参数
ROLE="manager"
PORT=8080
INSTALL_DIR="/opt/dist-log-analyzer"
DATA_DIR=""
MANAGER_URL="http://127.0.0.1:8080"
CLUSTER_TOKEN="dist-log-cluster-secret-token"
NODE_NAME=""
AUTO_START=true

# 解析命令行参数
while [[ $# -gt 0 ]]; do
    case "$1" in
        --role=*)
            ROLE="${1#*=}"
            ;;
        --port=*)
            PORT="${1#*=}"
            ;;
        --install-dir=*)
            INSTALL_DIR="${1#*=}"
            ;;
        --data-dir=*)
            DATA_DIR="${1#*=}"
            ;;
        --manager-url=*)
            MANAGER_URL="${1#*=}"
            ;;
        --cluster-token=*)
            CLUSTER_TOKEN="${1#*=}"
            ;;
        --node-name=*)
            NODE_NAME="${1#*=}"
            ;;
        --disk=*)
            DISK_DEVICE="${1#*=}"
            ;;
        --fstype=*)
            FS_TYPE="${1#*=}"
            ;;
        --format)
            FORMAT_DISK=true
            ;;
        --no-start)
            AUTO_START=false
            ;;
        -h|--help)
            echo "分布式存储日志分析系统 一键安装程序"
            echo "使用方法: $0 [选项]"
            echo ""
            echo "选项:"
            echo "  --role=manager|worker      安装角色: manager(管理组件, 默认) 或 worker(业务组件)"
            echo "  --port=<端口>              监听端口 (manager 默认 8080, worker 默认 8081)"
            echo "  --install-dir=<目录>       程序安装目标路径 (默认: /opt/dist-log-analyzer)"
            echo "  --data-dir=<目录>          数据存储与隔离根目录 (默认: <安装路径>/data)"
            echo "  --disk=<设备路径>          指定存放日志的物理硬盘 (例如: /dev/sdb)"
            echo "  --fstype=ext4|xfs          硬盘文件系统格式 (默认: ext4)"
            echo "  --format                   自动格式化该指定硬盘并配置开机自动挂载"
            echo "  --manager-url=<URL>        业务节点连接的管理节点地址 (worker 角色必填)"
            echo "  --cluster-token=<Token>    集群通信握手安全凭据"
            echo "  --node-name=<名称>         节点显示名称 (默认: 本机主机名)"
            echo "  --no-start                 安装完成后不立即启动服务"
            exit 0
            ;;
        *)
            # 支持直接传 role，如 ./install.sh manager 或 ./install.sh worker
            if [ "$1" == "manager" ] || [ "$1" == "worker" ]; then
                ROLE="$1"
            else
                echo "[WARN] 未知参数: $1"
            fi
            ;;
    esac
    shift
done

if [ -z "$DATA_DIR" ]; then
    DATA_DIR="$INSTALL_DIR/data"
fi

if [ "$ROLE" == "worker" ] && [ "$PORT" -eq 8080 ]; then
    PORT=8081
fi

IS_ROOT=false
if [ "$(id -u)" -eq 0 ]; then
    IS_ROOT=true
fi

# 如果不是 root 用户且默认安装在 /opt，降级到当前用户主目录下
if [ "$IS_ROOT" = false ] && [[ "$INSTALL_DIR" == "/opt"* ]]; then
    INSTALL_DIR="$HOME/.dist-log-analyzer"
    DATA_DIR="$INSTALL_DIR/data"
    echo "[INFO] 当前为非 root 用户，自动调整安装目录至: $INSTALL_DIR"
fi

echo "=================================================================="
echo "    分布式存储日志分析系统 (Distributed Storage Log Analyzer)     "
echo "    一键快速安装部署程序                                          "
echo "=================================================================="
echo "  ▶ 安装组件角色: $ROLE"
echo "  ▶ 程序安装路径: $INSTALL_DIR"
echo "  ▶ 数据存储路径: $DATA_DIR"
echo "  ▶ 服务监听端口: $PORT"
echo "=================================================================="

# 1. 查找二进制程序
SRC_BIN=""
if [ -f "$SCRIPT_DIR/bin/dist-log-analyzer" ]; then
    SRC_BIN="$SCRIPT_DIR/bin/dist-log-analyzer"
elif [ -f "$SCRIPT_DIR/dist-log-analyzer" ]; then
    SRC_BIN="$SCRIPT_DIR/dist-log-analyzer"
elif [ -f "$CURRENT_DIR/bin/dist-log-analyzer" ]; then
    SRC_BIN="$CURRENT_DIR/bin/dist-log-analyzer"
else
    echo "[ERROR] 未找到 dist-log-analyzer 可执行二进制，请确保安装包完整！"
    exit 1
fi

# 2. 停止可能运行的旧服务
echo "[1/4] 检查并停止可能运行的旧版本服务..."
pkill -f "$INSTALL_DIR/bin/dist-log-analyzer $ROLE" 2>/dev/null || true

# 3. 创建目录结构并部署文件
echo "[2/4] 部署文件到目标目录: $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/conf" "$INSTALL_DIR/logs" "$INSTALL_DIR/run" "$DATA_DIR"

cp -f "$SRC_BIN" "$INSTALL_DIR/bin/dist-log-analyzer"
chmod +x "$INSTALL_DIR/bin/dist-log-analyzer"

mkdir -p "$INSTALL_DIR/scripts"
if [ -f "$SCRIPT_DIR/scripts/service.sh" ]; then
    cp -f "$SCRIPT_DIR/scripts/service.sh" "$INSTALL_DIR/service.sh"
    cp -f "$SCRIPT_DIR/scripts/service.sh" "$INSTALL_DIR/scripts/service.sh"
    chmod +x "$INSTALL_DIR/service.sh" "$INSTALL_DIR/scripts/service.sh"
elif [ -f "$SCRIPT_DIR/service.sh" ]; then
    cp -f "$SCRIPT_DIR/service.sh" "$INSTALL_DIR/service.sh"
    cp -f "$SCRIPT_DIR/service.sh" "$INSTALL_DIR/scripts/service.sh"
    chmod +x "$INSTALL_DIR/service.sh" "$INSTALL_DIR/scripts/service.sh"
fi

# 4. 获取本机 IP
LOCAL_IP="127.0.0.1"
if command -v hostname >/dev/null 2>&1; then
    IP_CANDIDATE=$(hostname -I 2>/dev/null | awk '{print $1}')
    if [ -n "$IP_CANDIDATE" ]; then
        LOCAL_IP="$IP_CANDIDATE"
    fi
fi

# 5. 配置 Systemd 服务 (仅 root 用户且支持 systemd)
SYSTEMD_ENABLED=false
if [ "$IS_ROOT" = true ] && command -v systemctl >/dev/null 2>&1 && [ -d "/etc/systemd/system" ]; then
    echo "[3/4] 注册 Systemd 守护服务..."
    SERVICE_NAME="dist-log-$ROLE.service"
    SERVICE_PATH="/etc/systemd/system/$SERVICE_NAME"

    if [ "$ROLE" == "manager" ]; then
        EXEC_CMD="$INSTALL_DIR/bin/dist-log-analyzer manager --port=$PORT --data-dir=$DATA_DIR --advertise-ip=$LOCAL_IP --cluster-token=$CLUSTER_TOKEN"
    else
        EXEC_CMD="$INSTALL_DIR/bin/dist-log-analyzer worker --port=$PORT --manager-url=$MANAGER_URL --data-dir=$DATA_DIR --advertise-ip=$LOCAL_IP --cluster-token=$CLUSTER_TOKEN"
    fi

    cat > "$SERVICE_PATH" <<EOF
[Unit]
Description=Distributed Storage Log Analyzer ($ROLE)
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$EXEC_CMD
Restart=always
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
    SYSTEMD_ENABLED=true
fi

# 6. 启动服务
if [ "$AUTO_START" = true ]; then
    echo "[4/4] 启动分布式日志分析系统服务..."
    if [ "$SYSTEMD_ENABLED" = true ]; then
        systemctl restart "dist-log-$ROLE.service"
        sleep 1
        systemctl status "dist-log-$ROLE.service" --no-pager | head -n 8
        # 独立脚本方式常驻后台启动
        nohup "$INSTALL_DIR/bin/dist-log-analyzer" "$ROLE" \
            --port="$PORT" \
            --data-dir="$DATA_DIR" \
            --advertise-ip="$LOCAL_IP" \
            --cluster-token="$CLUSTER_TOKEN" </dev/null >> "$INSTALL_DIR/logs/$ROLE.log" 2>&1 &
        DAEMON_PID=$!
        disown $DAEMON_PID 2>/dev/null || true
        echo $DAEMON_PID > "$INSTALL_DIR/run/service.pid"
        sleep 1
    fi
fi

echo ""
echo "=================================================================="
echo "          🎉 安装完成！系统已成功部署并运行                       "
echo "=================================================================="
if [ "$ROLE" == "manager" ]; then
    echo "  ▶ 控制台 Web 访问地址: http://$LOCAL_IP:$PORT"
    echo "  ▶ 初始管理员账号: admin"
    echo "  ▶ 初始管理员密码: admin123"
    echo "  ▶ 数据存储隔离目录: $DATA_DIR"
    echo "  ▶ 业务组件后续安装: 登录 Web 控制台 -> [集群节点] -> 一键远程安装"
else
    echo "  ▶ 业务计算节点已接入: $LOCAL_IP:$PORT"
    echo "  ▶ 所属管理节点: $MANAGER_URL"
fi
echo ""
if [ "$SYSTEMD_ENABLED" = true ]; then
    echo "  ▶ 服务控制命令:"
    echo "     systemctl status dist-log-$ROLE"
    echo "     systemctl restart dist-log-$ROLE"
    echo "     systemctl stop dist-log-$ROLE"
else
    echo "  ▶ 服务控制命令:"
    echo "     $INSTALL_DIR/service.sh status"
    echo "     $INSTALL_DIR/service.sh restart"
    echo "     $INSTALL_DIR/service.sh stop"
fi
echo "=================================================================="
