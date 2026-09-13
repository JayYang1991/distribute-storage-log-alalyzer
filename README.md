# 分布式存储日志分析系统 (Distributed Storage Log Analyzer)

一套专为分布式存储系统（Ceph、HDFS、MinIO、GlusterFS 以及通用 Linux 存储 I/O）设计的轻量级、高性能分布式日志分析与智能故障诊断系统。

## ✨ 核心特性

1. **多节点分布式部署**：
   - 采用 Manager-Worker 分布式主从架构，支持 1 个管理节点与 N 个业务分析节点水平横向扩展。
   - 业务节点实时上报心跳、CPU、内存与磁盘负载，管理节点自动感知健康状态并进行任务调度分派。
   - 支持单节点自闭环运行（无 Worker 时 Manager 自动以本地引擎处理），亦支持多节点分布式并发处理。

2. **零外部依赖 & 原生适配通用 RedHat 衍生 Linux**：
   - **完全采用 Go 语言静态编译（`CGO_ENABLED=0`，目标 `linux/amd64`）**，彻底脱离系统 glibc 版本限制。
   - 无须在目标机预装 Python 运行环境、Node.js、Java、Docker、Elasticsearch 或外部数据库。
   - 前端 Web 资产通过 Go `embed.FS` 打包进单一 ELF 二进制文件，可在 RHEL / CentOS 7/8/9、Rocky Linux、AlmaLinux 等任何 Linux 环境下**直接解压一键运行**。

3. **一键安装管理组件 (Manager)**：
   - 安装包根目录下提供 `install.sh` 脚本，运行一条命令即可完成管理组件安装与开机自启配置。
   - 自动适配 Systemd 守护服务（root 用户）或便携常驻进程（非 root 用户）。
   - 初始化系统内置管理员账号 `admin / admin123`，并打印清晰的 Web 访问地址。

4. **Web 界面一键安装业务组件 (Worker)**：
   - 管理员登录 Web 控制台后，在【集群节点与安装】页面直接输入目标主机 IP、SSH 端口及凭据，点击【一键安装】；
   - 管理节点通过内置纯 Go SSH/SFTP 客户端自动推送业务二进制组件并启动服务，自动接入集群！
   - 同时支持生成离线一键命令（`curl http://<manager-ip>:<port>/api/agent/install.sh | bash`），方便在无密码环境下快速接入。

5. **多用户管理与独立存储空间物理隔离**：
   - 完整的 RBAC 权限控制，管理员可创建多用户并分配独立存储配额（如 1GB、10GB、50GB 或不限制）。
   - **物理目录与逻辑沙箱隔离**：每个用户拥有独立的数据目录（`data/users/<username>/archives/` 和 `data/users/<username>/extracted/`），日志包、解压文件树、检索结果与诊断报告互不干扰。

6. **常用压缩包格式支持、多层嵌套解压与在线文件树浏览**：
   - 原生支持 `.tar.gz`, `.tgz`, `.zip`, `.7z`, `.tar`, `.gz`, `.bz2` 等常见存储日志压缩格式与多层嵌套压缩包自动递归解压；
   - 解压后完整保留多层级目录结构与嵌套日志包展开目录，提供双栏式文件树浏览器，支持在线大文本分页高亮阅读。

7. **高性能分布式全文与正则检索**：
   - 支持在归档日志中进行关键词匹配、正则表达式匹配、日志级别（DEBUG/INFO/WARN/ERROR/FATAL）过滤以及文件路径过滤；
   - 支持**上下文展开查看（Context View）**，点击即可查看命中行前后的关联日志，快速定位故障上下文；
   - 任务支持分派至分布式 Worker 并发执行。

8. **预设存储故障规则库与自动化诊断引擎**：
   - 内置针对 Ceph（OSD Crash、Slow Requests、PG Peering）、HDFS（DataNode dead、Corrupt Block）、MinIO（Drive offline）、Linux 磁盘 I/O 错误（Buffer I/O error、只读文件系统挂载）等专业的规则特征库；
   - 管理员可在 Web 界面新增/编辑/启用/禁用自定义规则；
   - 用户上传日志后自动触发异步智能诊断，生成直观的可视化【故障诊断报告】，包含系统健康评分（0-100分）、严重程度分类统计、故障时序流水、代码/日志精准定位与专家排查处置建议。

9. **业务组件异常监控与实时告警中心**：
   - **业务组件自主健康巡检**：Worker 节点持续对存储挂载盘可用空间（<1GB 触发 `DISK_FULL` 严重告警）、文件系统写入与只读异常（触发 `DISK_READONLY` 严重告警）、系统高内存过载（>92% 触发 `HIGH_MEMORY` 警告）以及任务执行失败进行检测；
   - **异常事件主动上报与聚合消抖**：异常发生时实时上报至管理节点，支持多 Manager 故障自动转移与 30 秒限频聚合防风暴；
   - **节点失联探测与心跳自愈消警**：管理节点持续监控 Worker 节点存活性，失联超时自动生成 `NODE_OFFLINE` 严重告警；节点网络恢复并重新上报心跳时自动触发消警自愈；
   - **Web 控制台全生命周期管理**：顶部栏实时告警角标联动、仪表盘未解除告警大屏，独立【实时告警中心】支持按级别过滤、管理员确认、手动解除与一键清理历史。

10. **一键打包生成发布包**：
    - 项目提供 `package.sh` 脚本，一键构建静态二进制并打包为自包含的 `dist-log-analyzer-linux-amd64.tar.gz`（体积仅 ~3.0MB）与 SHA256 校验和。

11. **自定义时序图表脚本引擎与突变定位穿透 (Zero-Code Dynamic Charting)**：
    - **特定文件名规则绑定与本地脚本上传**：管理员在 Web 端可针对特定打点文件（如 `(?i).*iostat.*\.log$`、`**/vmstat*.txt`）配置解析脚本，支持**直接选择本地脚本文件（.py / .sh / .awk）一键上传导入**或在线编写微调；
    - **海量点位极值降采样 (LTTB / Min-Max)**：面对数十万长期打点日志，框架自动分桶压缩至 1500 点以内，**100% 保证瞬时高负荷毛刺（如利用率 100% 尖峰）绝不失真**；
    - **鼠标框选视口动态自适应提升精度**：在全景图上用鼠标拉框选中局部时段（如 14:20~14:25），系统自动从本地缓存无缝切片，**瞬间展开为未压缩的秒级原始高精度波动波形**；
    - **突变点双轨识别与一键平滑聚焦**：支持脚本主动返回或框架一阶差分斜率跃变（$|\Delta y| > 3\sigma$）自动识别突变点，顶部提供导航胶囊一键平滑聚焦；
    - **直达案发现场**：点击突变点悬浮气泡，一键穿透直达底层日志查看器并精准高亮对应的原始日志行号。

---

## 📈 自定义时序图表解析脚本引擎开发指南

### 1. 脚本调用契约规范
客户可以使用 Python、Shell、AWK 等任意语言编写解析脚本。脚本运行时的标准契约非常简单：
- **命令行参数**: `$1` 为系统传入的目标日志文件绝对路径（Worker 本地只读访问）；
- **标准输出 (stdout)**: 输出标准 JSON 字符串到 stdout。

#### 标准输出 JSON 协议格式：
```json
{
  "title": "iostat 磁盘 I/O 利用率时序分析",
  "description": "监控各磁盘利用率波动及高负荷突变",
  "x_axis": {
    "label": "采集时间",
    "type": "time",
    "data": ["14:00:01", "14:00:02", "14:00:03"]
  },
  "series": [
    {
      "name": "sda 利用率 (%util)",
      "unit": "%",
      "chart_type": "line",
      "data": [12.5, 98.2, 23.1]
    }
  ],
  "anomalies": [
    {
      "time": "14:00:02",
      "index": 1,
      "metric": "sda 利用率 (%util)",
      "value": 98.2,
      "severity": "CRITICAL",
      "reason": "设备 sda 磁盘利用率骤增达到危险高位 98.2%",
      "line_number": 45802
    }
  ]
}
```

### 2. Python 实战样例：20 行解析 `iostat -xz 1` 日志
```python
#!/usr/bin/env python3
import sys, re, json

log_file = sys.argv[1]
timestamps, dev_util, anomalies = [], {}, []
current_time, line_idx = "", 0
re_time = re.compile(r"^(\\d{2}:\\d{2}:\\d{2}|\\d{4}-\\d{2}-\\d{2}[ T]\\d{2}:\\d{2}:\\d{2})")

with open(log_file, "r", encoding="utf-8", errors="ignore") as f:
    for raw in f:
        line_idx += 1
        line = raw.strip()
        tm = re_time.match(line)
        if tm:
            current_time = tm.group(1)
            if current_time not in timestamps: timestamps.append(current_time)
            continue
        parts = line.split()
        if len(parts) >= 12 and not parts[0].startswith("Device") and not parts[0].startswith("avg-cpu"):
            dev, util = parts[0], float(parts[-1])
            dev_util.setdefault(dev, []).append(util)
            if util >= 85.0:
                anomalies.append({
                    "time": current_time, "index": len(timestamps)-1,
                    "metric": f"{dev} 利用率", "value": util,
                    "severity": "CRITICAL" if util >= 95.0 else "WARNING",
                    "reason": f"磁盘利用率突变达到 {util}%", "line_number": line_idx
                })

series = [{"name": f"{d} 利用率", "unit": "%", "data": dev_util[d]} for d in sorted(dev_util.keys())[:8]]
print(json.dumps({
    "title": "iostat 磁盘利用率分析",
    "x_axis": {"label": "采样时间", "data": timestamps},
    "series": series,
    "anomalies": anomalies
}))
```

---

## 🚀 快速上手

### 1. 一键本地生成安装包
在开发环境或编译机执行：
```bash
./package.sh
```
执行后将在 `dist/` 目录下生成发布包：
- `dist/dist-log-analyzer-linux-amd64.tar.gz`
- `dist/dist-log-analyzer-linux-amd64.tar.gz.sha256`

### 2. 推送 Git Tag 触发 GitHub 自动编译生成 Release (免 Token)
项目已内置全自动 GitHub Actions CI/CD 工作流（`.github/workflows/release.yml`）。
开发者在本地**无需配置任何 GitHub Token**，只需通过 Git 推送版本 Tag，即可自动触发 GitHub 云端多环境编译、打包并自动创建 GitHub Release 挂载安装包资产：

```bash
# 1. 提交当前代码
git add .
git commit -m "feat: 发布 v1.0.0 正式版"
git push origin main

# 2. 打上版本标签并推送 (自动触发 GitHub Actions 自动构建与发布)
git tag v1.0.0
git push origin v1.0.0
```
推送后即可在 GitHub 仓库的 **Releases** 页面查看自动生成的发布包与安装说明。

### 3. 目标服务器一键安装管理组件 (极简部署)
将生成的 `dist-log-analyzer-linux-amd64.tar.gz` 拷贝至任意通用 RedHat 衍生 Linux（或 CentOS / Rocky / AlmaLinux / Ubuntu 等）机器：
```bash
tar -zxvf dist-log-analyzer-linux-amd64.tar.gz
cd dist-log-analyzer-linux-amd64

# 一键极简安装并启动管理组件 (默认端口 8080，无复杂命令行参数交互)
sudo ./install.sh

# 或仅按需指定基础系统参数 (如端口与数据存放根目录):
sudo ./install.sh --port=8080 --data-dir=/opt/dist-log/data
```

安装完成后即可在浏览器打开 Web 控制台：
- **访问地址**: `http://<服务器IP>:8080`
- **默认管理员账号**: `admin`
- **默认管理员密码**: `admin123`

> 💡 **免安装复杂传参设计**：
> 高可用架构模式切换 (单机 Standalone / HA Primary / HA Backup)、对端管理节点同步地址、默认网关 IP (`--gateway-ip`)、双重防脑裂仲裁开关、虚拟高可用 IP (VIP) 以及心跳超时等所有高级参数，**均已全面集成在 Web 控制台进行可视化配置**！
> 登录 Web 控制台后，点击【集群节点与安装】->【⚙️ 高可用与网络配置】即可随时调整，点击保存立即**热生效**，无需在终端安装时指定繁杂参数或重启服务。

### 4. 在 Web 界面安装业务计算节点
1. 登录 Web 控制台，点击左侧导航【集群节点与安装】；
2. 点击【➕ 一键安装业务组件 (SSH)】，填写目标业务服务器 IP、端口及 SSH 账号密码；
3. 点击【开始一键安装并接入】，系统通过内置 SSH 自动完成传输与服务启动，业务节点自动向管理节点注册上线！
4. 亦可点击【离线接入命令】，在目标机器终端粘贴运行一键脚本接入。

### 5. 一键卸载与环境清理
系统提供全自动安全卸载脚本，可自动停止与注销 Systemd 守护服务、清理运行进程并清除程序文件：

```bash
# 1. 安全卸载 (注销服务并清理程序二进制，默认安全保留历史分析数据与数据库):
sudo ./uninstall.sh

# 2. 彻底卸载 (连同数据目录 data/、用户上传的归档日志包及 SQLite 数据库一并清空):
sudo ./uninstall.sh --purge -y

# 3. 指定自定义安装目录卸载:
sudo ./uninstall.sh --install-dir=/opt/dist-log-analyzer-manager
```

---

## 📁 目录结构说明

```
.
├── bin/                        # 编译生成的可执行文件
│   └── dist-log-analyzer       # 纯静态单二进制，内嵌 Web 前端
├── cmd/
│   └── analyzer/               # 主入口程序 (manager / worker / version 子命令)
├── internal/
│   ├── config/                 # 系统配置管理
│   ├── manager/                # 管理节点核心 (Web API, 调度器, 内置 SSH 远程部署器)
│   ├── model/                  # 数据结构定义 (用户, 节点, 归档包, 规则, 诊断事件)
│   ├── rules/                  # 故障诊断规则库与匹配引擎
│   ├── store/                  # 纯 Go 本地数据存储与用户沙箱物理空间隔离
│   ├── web/                    # 现代化深色响应式 Web 前端 (embed 嵌入)
│   └── worker/                 # 业务计算节点核心 (解压缩, 全文检索, 资源上报)
├── scripts/
│   ├── install.sh              # 统一安装脚本 (适配 manager 与 worker)
│   ├── uninstall.sh            # 一键安全/彻底卸载与环境清理脚本
│   └── service.sh              # 服务管理与进程守护控制脚本
├── package.sh                  # 一键打包生成免依赖发布压缩包脚本
├── install.sh                  # 根目录快捷安装入口
├── uninstall.sh                # 根目录快捷卸载入口
├── go.mod                      # 纯 Go 模块定义
└── README.md                   # 系统说明文档
```
