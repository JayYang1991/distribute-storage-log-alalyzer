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
HA_MODE="standalone"
PEER_URL=""
VIP=""
VIP_INTERFACE=""

# 解析命令行参数
while [[ $# -gt 0 ]]; do
    case "$1" in
        --role=*|--component=*)
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
        --ha-mode=*)
            HA_MODE="${1#*=}"
            ;;
        --peer-url=*)
            PEER_URL="${1#*=}"
            ;;
        --vip=*)
            VIP="${1#*=}"
            ;;
        --vip-interface=*)
            VIP_INTERFACE="${1#*=}"
            ;;
        --gateway-ip=*)
            GATEWAY_IP="${1#*=}"
            ;;
        --manager-url=*|--manager=*)
            MANAGER_URL="${1#*=}"
            ;;
        --cluster-token=*)
            CLUSTER_TOKEN="${1#*=}"
            ;;
        --node-name=*)
            NODE_NAME="${1#*=}"
            ;;
        --disk=*|--disks=*)
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
        -y|--yes)
            # 无需确认
            ;;
        -h|--help)
            echo "分布式存储日志分析系统 一键安装程序"
            echo "使用方法: $0 [选项]"
            echo ""
            echo "安装时核心选项:"
            echo "  --role=manager|worker      安装角色: manager(管理组件, 默认) 或 worker(业务组件)"
            echo "  --port=<端口>              监听端口 (manager 默认 8080, worker 默认 8081)"
            echo "  --install-dir=<目录>       程序安装目标路径 (默认: /opt/dist-log-analyzer)"
            echo "  --data-dir=<目录>          数据存储与隔离根目录 (默认: <安装路径>/data)"
            echo "  --manager-url=<URL>        连接的管理节点地址 (仅 worker 角色需要，支持主备多地址)"
            echo "  --cluster-token=<Token>    集群通信安全凭据"
            echo "  --node-name=<名称>         节点显示名称 (默认: 本机主机名)"
            echo "  --no-start                 安装完成后不立即启动服务"
            echo ""
            echo "业务存储盘选项 (仅 worker 角色支持):"
            echo "  --disk=<设备路径>          指定存放日志的物理硬盘 (例如: /dev/sdb)"
            echo "  --fstype=ext4|xfs          硬盘文件系统格式 (默认: ext4)"
            echo "  --format                   自动格式化该指定硬盘并配置开机自动挂载"
            echo ""
            echo "💡 提示: 高可用 HA 架构、对端节点同步、网关防脑裂自检 (--gateway-ip)、虚拟 IP (VIP) 等高级参数，"
            echo "         均已全部移至 Web 管理控制台进行图形化配置并支持在线热生效，无需在安装时复杂指定！"
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

# 初始化安装日志记录 (在任何服务及 systemd 启动前即时开启全链路日志记录，避免异常无法定位)
INSTALL_LOG="/tmp/dist-log-install.log"
mkdir -p "$(dirname "$INSTALL_LOG")" 2>/dev/null || true
{
    echo "=================================================================="
    echo "  分布式存储日志分析系统 安装部署详细日志"
    echo "  启动时间: $(date '+%Y-%m-%d %H:%M:%S')"
    echo "  执行用户: $(whoami) (UID: $(id -u))"
    echo "  主机名称: $(hostname 2>/dev/null || echo 'unknown')"
    echo "  操作系统: $(uname -srm 2>/dev/null || echo 'unknown')"
    echo "  脚本路径: $0"
    echo "  执行目录: $CURRENT_DIR"
    echo "  配置参数: ROLE=$ROLE PORT=$PORT INSTALL_DIR=$INSTALL_DIR DATA_DIR=$DATA_DIR AUTO_START=$AUTO_START"
    echo "=================================================================="
} >> "$INSTALL_LOG"

# 备份原生文件描述符，确保退出时安全 flush 管道
exec 3>&1 4>&2
exec > >(tee -a "$INSTALL_LOG") 2>&1

# 退出捕获机制 (统一处理 set -e 异常退出与显式 exit 1)
on_install_exit() {
    local exit_code=$?
    # 还原文件描述符，断开管道并等待 tee 完全写入落盘
    exec 1>&3 2>&4 2>/dev/null || true
    wait 2>/dev/null || true

    if [ "$exit_code" -ne 0 ]; then
        echo ""
        echo "❌ [安装终止] 脚本执行未完成即退出 (退出码: $exit_code)！"
        echo "  ▶ 完整安装诊断日志已保存于: $INSTALL_LOG"
        if [ -d "$INSTALL_DIR/logs" ]; then
            cp -f "$INSTALL_LOG" "$INSTALL_DIR/logs/install.log" 2>/dev/null || true
            echo "  ▶ 诊断日志已同步至: $INSTALL_DIR/logs/install.log"
        fi
        echo "  ▶ 退出前最近 25 行执行日志如下:"
        echo "------------------------------------------------------------------"
        tail -n 25 "$INSTALL_LOG" 2>/dev/null || true
        echo "------------------------------------------------------------------"
    fi
}
trap 'on_install_exit' EXIT

echo "=================================================================="
echo "    分布式存储日志分析系统 (Distributed Storage Log Analyzer)     "
echo "    一键快速安装部署程序                                          "
echo "=================================================================="
echo "  ▶ 安装组件角色: $ROLE"
echo "  ▶ 程序安装路径: $INSTALL_DIR"
echo "  ▶ 数据存储路径: $DATA_DIR"
echo "  ▶ 服务监听端口: $PORT"
echo "  ▶ 安装过程日志: $INSTALL_LOG"
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

# 2. Worker 角色强制物理磁盘安全检查与多盘解析
DISK_LIST=()
if [ "$ROLE" == "worker" ]; then
    if [ -z "$DISK_DEVICE" ]; then
        echo "=================================================================="
        echo "  ❌ [安全策略拦截] 严禁使用系统盘存放日志！"
        echo "=================================================================="
        echo "  架构安全规范: 业务 Worker 计算节点必须使用独立的物理数据盘，"
        echo "  严禁将分析日志、解压文件与系统根分区混合存放以防写满宕机。"
        echo ""
        echo "  请通过 --disk 参数指定至少一块独立物理硬盘，支持单盘或多盘:"
        echo "    单盘示例: sudo ./install.sh --role=worker --disk=/dev/sdb --format"
        echo "    多盘示例: sudo ./install.sh --role=worker --disk=/dev/sdb,/dev/sdc --format"
        echo "=================================================================="
        exit 1
    fi

    # 解析可能以逗号分隔的多块物理硬盘
    IFS=',' read -r -a RAW_DISKS <<< "$DISK_DEVICE"
    for d in "${RAW_DISKS[@]}"; do
        clean_d=$(echo "$d" | xargs)
        if [ -n "$clean_d" ]; then
            DISK_LIST+=("$clean_d")
        fi
    done

    if [ ${#DISK_LIST[@]} -eq 0 ]; then
        echo "[ERROR] 未解析到有效的物理硬盘设备路径！"
        exit 1
    fi

    # 逐一执行系统盘防呆检测
    for d in "${DISK_LIST[@]}"; do
        if command -v lsblk >/dev/null 2>&1; then
            for mp in $(lsblk -n -o MOUNTPOINT "$d" 2>/dev/null); do
                if [ "$mp" == "/" ] || [ "$mp" == "/boot" ] || [[ "$mp" == "/boot/"* ]] || [ "$mp" == "/home" ] || [ "$mp" == "/usr" ] || [ "$mp" == "/var" ] || [[ "$mp" == *swap* ]] || [[ "$mp" == *SWAP* ]]; then
                    echo "=================================================================="
                    echo "  🚫 [安全防呆拦截] 严禁使用系统关键磁盘！"
                    echo "=================================================================="
                    echo "  检测到目标设备 $d 关联系统关键分区 ($mp)！"
                    echo "  为保证操作系统稳定与数据安全，严禁使用系统盘作为日志存储盘。"
                    echo "  请选择未被操作系统占用的独立物理数据盘 (如 /dev/sdb)。"
                    echo "=================================================================="
                    exit 1
                fi
            done
        fi
    done
fi

# 3. 停止可能运行的旧服务
echo "[1/4] 检查并停止可能运行的旧版本服务..."
pkill -f "$INSTALL_DIR/bin/dist-log-analyzer $ROLE" 2>/dev/null || true

# 4. 创建目录结构并部署文件
echo "[2/4] 部署文件到目标目录: $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/conf" "$INSTALL_DIR/logs" "$INSTALL_DIR/run"

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

if [ -f "$SCRIPT_DIR/scripts/uninstall.sh" ]; then
    cp -f "$SCRIPT_DIR/scripts/uninstall.sh" "$INSTALL_DIR/uninstall.sh"
    cp -f "$SCRIPT_DIR/scripts/uninstall.sh" "$INSTALL_DIR/scripts/uninstall.sh"
    chmod +x "$INSTALL_DIR/uninstall.sh" "$INSTALL_DIR/scripts/uninstall.sh"
elif [ -f "$SCRIPT_DIR/uninstall.sh" ]; then
    cp -f "$SCRIPT_DIR/uninstall.sh" "$INSTALL_DIR/uninstall.sh"
    cp -f "$SCRIPT_DIR/uninstall.sh" "$INSTALL_DIR/scripts/uninstall.sh"
    chmod +x "$INSTALL_DIR/uninstall.sh" "$INSTALL_DIR/scripts/uninstall.sh"
fi

# 5. 获取本机 IP
LOCAL_IP="127.0.0.1"
if command -v hostname >/dev/null 2>&1; then
    IP_CANDIDATE=$(hostname -I 2>/dev/null | awk '{print $1}')
    if [ -n "$IP_CANDIDATE" ]; then
        LOCAL_IP="$IP_CANDIDATE"
    fi
fi

# 6. 配置并启动服务 (多盘多进程支持)
SYSTEMD_ENABLED=false
if [ "$IS_ROOT" = true ] && command -v systemctl >/dev/null 2>&1 && [ -d "/etc/systemd/system" ]; then
    SYSTEMD_ENABLED=true
fi

INSTALLED_INSTANCES=()

if [ "$ROLE" == "manager" ]; then
    echo "[3/4] 注册 Manager Systemd 守护服务..."
    mkdir -p "$DATA_DIR"
    MGR_HA_ARGS="--ha-mode=$HA_MODE"
    if [ -n "$PEER_URL" ]; then MGR_HA_ARGS="$MGR_HA_ARGS --peer-url=$PEER_URL"; fi
    if [ -n "$VIP" ]; then MGR_HA_ARGS="$MGR_HA_ARGS --vip=$VIP"; fi
    if [ -n "$VIP_INTERFACE" ]; then MGR_HA_ARGS="$MGR_HA_ARGS --vip-interface=$VIP_INTERFACE"; fi
    if [ -n "$GATEWAY_IP" ]; then MGR_HA_ARGS="$MGR_HA_ARGS --gateway-ip=$GATEWAY_IP"; fi
    EXEC_CMD="$INSTALL_DIR/bin/dist-log-analyzer manager --port=$PORT --data-dir=$DATA_DIR --advertise-ip=$LOCAL_IP --cluster-token=$CLUSTER_TOKEN $MGR_HA_ARGS"

    if [ "$SYSTEMD_ENABLED" = true ]; then
        SERVICE_NAME="dist-log-manager.service"
        cat > "/etc/systemd/system/$SERVICE_NAME" <<EOF
[Unit]
Description=Distributed Storage Log Analyzer (manager)
After=network.target
StartLimitIntervalSec=60s
StartLimitBurst=10

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$EXEC_CMD
Restart=always
RestartSec=3s
LimitNOFILE=65536
TimeoutStopSec=15s
KillMode=mixed

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
        if [ "$AUTO_START" = true ]; then
            echo "[4/4] 启动 Manager 服务并验证健康状态..."
            systemctl restart "$SERVICE_NAME"
            sleep 1
            if systemctl is-active --quiet "$SERVICE_NAME"; then
                echo "  ✔ [健康自检通过] Systemd 服务 $SERVICE_NAME 处于 active 运行状态，故障 3 秒自动拉起 (Restart=always) 已生效！"
                systemctl status "$SERVICE_NAME" --no-pager | head -n 12
            else
                echo "  ❌ [健康自检失败] Systemd 服务 $SERVICE_NAME 未处于 active 运行状态！"
                echo "  ▶ 诊断信息 (systemctl status):"
                systemctl status "$SERVICE_NAME" --no-pager || true
                echo "  ▶ 最近服务日志 (journalctl -u $SERVICE_NAME -n 50 --no-pager):"
                journalctl -u "$SERVICE_NAME" -n 50 --no-pager || true
                exit 1
            fi
        fi
    else
        if [ "$AUTO_START" = true ]; then
            echo "[4/4] 启动 Manager 进程..."
            nohup $EXEC_CMD </dev/null >> "$INSTALL_DIR/logs/manager.log" 2>&1 &
            echo $! > "$INSTALL_DIR/run/manager.pid"
        fi
    fi
    INSTALLED_INSTANCES+=("Manager: http://$LOCAL_IP:$PORT")

else
    # Worker 角色：为每块物理硬盘启动独立的 Worker 进程实例 (多进程隔离架构)
    echo "[3/4] 为选定的 ${#DISK_LIST[@]} 块物理存储盘配置专属 Worker 独立进程与挂载点..."
    BASE_STORAGE_MOUNT="/data/dist-log-storage"

    for idx in "${!DISK_LIST[@]}"; do
        disk_dev="${DISK_LIST[$idx]}"
        disk_name=$(basename "$disk_dev")
        inst_port=$((PORT + idx))
        inst_name="${NODE_NAME:-$(hostname)}-$disk_name"
        inst_mount="$BASE_STORAGE_MOUNT/disk-$disk_name"
        inst_data_dir="$inst_mount/data"

        echo "  --------------------------------------------------"
        echo "  ▶ 正在配置第 $((idx + 1))/${#DISK_LIST[@]} 块磁盘: $disk_dev"
        echo "     实例名称: $inst_name"
        echo "     服务端口: $inst_port"
        echo "     专用挂载点: $inst_mount"

        # 检查是否格式化与挂载
        if [ "$IS_ROOT" = true ]; then
            mkdir -p "$inst_mount"
            # 检查该磁盘是否已被挂载
            if ! grep -qs "$inst_mount" /proc/mounts; then
                if [ "$FORMAT_DISK" = true ]; then
                    echo "     ⚡ 正在执行磁盘格式化 ($FS_TYPE: $disk_dev)..."
                    umount "$disk_dev" 2>/dev/null || true
                    if [ "$FS_TYPE" == "xfs" ]; then
                        mkfs.xfs -f "$disk_dev" >/dev/null 2>&1 || true
                    else
                        mkfs.ext4 -F "$disk_dev" >/dev/null 2>&1 || true
                    fi
                fi
                echo "     正在挂载磁盘 $disk_dev 到 $inst_mount..."
                mount "$disk_dev" "$inst_mount" 2>/dev/null || mount -o defaults "$disk_dev" "$inst_mount" 2>/dev/null || true

                # 写入 /etc/fstab (若未存在)
                if ! grep -qs "$inst_mount" /etc/fstab; then
                    echo "$disk_dev $inst_mount $FS_TYPE defaults 0 0" >> /etc/fstab
                fi
            else
                echo "     ✔ 磁盘已处于挂载状态 ($inst_mount)"
            fi
        fi

        mkdir -p "$inst_data_dir"
        inst_exec="$INSTALL_DIR/bin/dist-log-analyzer worker --port=$inst_port --manager-url=$MANAGER_URL --data-dir=$inst_data_dir --advertise-ip=$LOCAL_IP --cluster-token=$CLUSTER_TOKEN --node-name=$inst_name"

        if [ "$SYSTEMD_ENABLED" = true ]; then
            svc_file="dist-log-worker-$disk_name.service"
            cat > "/etc/systemd/system/$svc_file" <<EOF
[Unit]
Description=Distributed Storage Log Analyzer Worker (Disk: $disk_dev, Port: $inst_port)
After=network.target
StartLimitIntervalSec=60s
StartLimitBurst=10

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$inst_exec
Restart=always
RestartSec=3s
LimitNOFILE=65536
TimeoutStopSec=15s
KillMode=mixed

[Install]
WantedBy=multi-user.target
EOF
            systemctl daemon-reload
            systemctl enable "$svc_file" >/dev/null 2>&1 || true
            if [ "$AUTO_START" = true ]; then
                systemctl restart "$svc_file"
            fi
        else
            if [ "$AUTO_START" = true ]; then
                nohup $inst_exec </dev/null >> "$INSTALL_DIR/logs/worker-$disk_name.log" 2>&1 &
                echo $! > "$INSTALL_DIR/run/worker-$disk_name.pid"
            fi
        fi
        INSTALLED_INSTANCES+=("Worker 实例 (磁盘: $disk_dev): $LOCAL_IP:$inst_port -> 挂载点: $inst_mount")
    done

    if [ "$AUTO_START" = true ]; then
        sleep 1
        echo "[4/4] 验证所有 Worker 实例服务健康状态..."
        if [ "$SYSTEMD_ENABLED" = true ]; then
            ALL_HEALTHY=true
            for idx in "${!DISK_LIST[@]}"; do
                d_name=$(basename "${DISK_LIST[$idx]}")
                s_file="dist-log-worker-$d_name.service"
                if systemctl is-active --quiet "$s_file"; then
                    echo "  ✔ [健康自检通过] Systemd 守护服务 $s_file 处于 active 运行状态，故障 3 秒自动拉起 (Restart=always) 已生效！"
                else
                    echo "  ❌ [健康自检失败] Systemd 守护服务 $s_file 未处于 active 状态！"
                    echo "  ▶ 诊断信息 (systemctl status):"
                    systemctl status "$s_file" --no-pager || true
                    echo "  ▶ 最近服务日志 (journalctl -u $s_file -n 50 --no-pager):"
                    journalctl -u "$s_file" -n 50 --no-pager || true
                    ALL_HEALTHY=false
                fi
            done
            if [ "$ALL_HEALTHY" != true ]; then
                echo "[ERROR] 部分 Worker 实例启动健康检查未通过，请检查日志！"
                exit 1
            fi
        fi
    fi
fi

# 安装日志归档保存
if [ -d "$INSTALL_DIR/logs" ]; then
    cp -f "$INSTALL_LOG" "$INSTALL_DIR/logs/install.log" 2>/dev/null || true
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
    echo "  ▶ ⚙️ 高可用与网络配置: 登录 Web 控制台 -> [集群节点] -> [⚙️ 高可用与网络配置]"
    echo "  ▶ 业务组件后续安装: 登录 Web 控制台 -> [集群节点] -> 一键远程安装 (SSH)"
    echo ""
    if [ "$SYSTEMD_ENABLED" = true ]; then
        echo "  ▶ 服务控制命令:"
        echo "     systemctl status dist-log-manager"
        echo "     systemctl restart dist-log-manager"
        echo "     systemctl stop dist-log-manager"
    fi
else
    echo "  ▶ 业务节点所属管理端: $MANAGER_URL"
    echo "  ▶ 本机成功拉起 ${#DISK_LIST[@]} 个独立物理磁盘的 Worker 存储进程:"
    for inst in "${INSTALLED_INSTANCES[@]}"; do
        echo "     ✔ $inst"
    done
    echo ""
    if [ "$SYSTEMD_ENABLED" = true ]; then
        echo "  ▶ 实例服务控制命令 (以第一块盘为例):"
        first_disk=$(basename "${DISK_LIST[0]}")
        echo "     systemctl status dist-log-worker-$first_disk"
        echo "     systemctl restart dist-log-worker-$first_disk"
        echo "     systemctl stop dist-log-worker-$first_disk"
    fi
fi
echo ""
echo "  ▶ 完整安装部署日志: $INSTALL_LOG"
if [ -f "$INSTALL_DIR/logs/install.log" ]; then
    echo "  ▶ 本地归档日志路径: $INSTALL_DIR/logs/install.log"
fi
echo "  ▶ 一键卸载命令:"
echo "     sudo $INSTALL_DIR/uninstall.sh           # 安全卸载 (保留历史数据)"
echo "     sudo $INSTALL_DIR/uninstall.sh --purge   # 彻底清除 (含数据与日志)"
echo "=================================================================="
