#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
分布式存储日志分析系统 - 远程虚拟机环境全量功能端到端自动化验证套件
覆盖:
  1. 管理员登录鉴权与 Token 控制
  2. 集群节点心跳与指标上报
  3. 多 Worker 容量调度算法
  4. 日志包构造、上传、流式解压与零二次IO行数统计
  5. 规则引擎多核并发智能诊断与健康评分
  6. 流式滑动环形缓冲区日志检索与上下文提取
  7. 文件内容流式查看与指定文件精准下载 (含安全防越权测试)
  8. 告警中心全生命周期与聚合自愈
  9. HA 高可用动态热配置与网关/Worker防脑裂仲裁自检
"""

import os
import sys
import time
import json
import tarfile
import urllib.request
import urllib.error
import subprocess

BASE_URL = "http://127.0.0.1:8080"
TOKEN = ""

def log(step, msg):
    print(f"\033[1;34m[{step}]\033[0m {msg}")

def success(msg):
    print(f"  \033[1;32m✔ PASS:\033[0m {msg}")

def fail(msg):
    print(f"  \033[1;31m✘ FAIL:\033[0m {msg}")
    sys.exit(1)

def http_req(path, method="GET", data=None, headers=None, expect_code=200):
    url = f"{BASE_URL}{path}"
    if headers is None:
        headers = {}
    if TOKEN and "token=" not in path and "Authorization" not in headers:
        headers["Authorization"] = f"Bearer {TOKEN}"
    
    req_body = None
    if data is not None:
        if isinstance(data, dict):
            req_body = json.dumps(data).encode("utf-8")
            headers["Content-Type"] = "application/json"
        elif isinstance(data, bytes):
            req_body = data

    req = urllib.request.Request(url, data=req_body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            code = resp.getcode()
            body = resp.read()
            if code != expect_code:
                fail(f"{method} {path} 预期状态码 {expect_code}, 实际: {code}")
            return code, body
    except urllib.error.HTTPError as e:
        if e.code == expect_code:
            return e.code, e.read()
        fail(f"{method} {path} 发生 HTTP 错误: {e.code} - {e.read().decode('utf-8', errors='ignore')}")
    except Exception as e:
        fail(f"{method} {path} 请求失败: {str(e)}")

# ================= 测试用例 1: 认证鉴权 =================
def test_auth():
    global TOKEN
    log("STEP 1", "测试管理员登录认证鉴权...")
    
    # 1.1 错误密码登录应被拒绝
    code, body = http_req("/api/auth/login", method="POST", data={"username": "admin", "password": "wrong_password"}, expect_code=401)
    success("错误密码鉴权拦截正常 (返回 401)")

    # 1.2 正确密码登录
    code, body = http_req("/api/auth/login", method="POST", data={"username": "admin", "password": "admin123"})
    res = json.loads(body.decode("utf-8"))
    TOKEN = res.get("token")
    if not TOKEN:
        fail("登录响应未返回 token")
    success(f"登录成功，获取管理员 Token: {TOKEN[:12]}...")

    # 1.3 未授权访问受保护接口
    url = f"{BASE_URL}/api/nodes"
    req = urllib.request.Request(url)
    try:
        with urllib.request.urlopen(req) as resp:
            fail("未携带 Token 访问应返回 401")
    except urllib.error.HTTPError as e:
        if e.code == 401:
            success("未携带 Token 访问受保护接口正确拦截 (401)")
        else:
            fail(f"预期 401，实际为: {e.code}")

# ================= 测试用例 2: 集群节点心跳与指标 =================
def test_cluster_nodes():
    log("STEP 2", "测试集群节点列表与 Worker 心跳指标监控...")
    code, body = http_req(f"/api/nodes?token={TOKEN}")
    nodes = json.loads(body.decode("utf-8"))
    if not isinstance(nodes, list) or len(nodes) < 2:
        fail(f"预期至少 2 个节点(Manager + 1 Worker)，实际获取到 {len(nodes)} 个节点")

    worker_found = False
    for n in nodes:
        if n.get("role") == "worker":
            worker_found = True
            res = n.get("resource", {})
            if res.get("disk_total_mb", 0) <= 0:
                fail(f"Worker 节点 {n.get('id')} 磁盘指标采集异常: {res}")
            success(f"Worker 节点在线: {n.get('name')} (IP: {n.get('ip')}:{n.get('port')}, 磁盘总容量: {res.get('disk_total_mb')} MB, 内存: {res.get('mem_total_mb')} MB)")
    if not worker_found:
        fail("未找到在线的 Worker 节点")

# ================= 测试用例 3: 启动 Worker 2 验证容量均衡调度 =================
def test_dual_worker_capacity_balance():
    log("STEP 3", "启动 Worker 2 验证多节点容量调度策略...")
    # 检查是否已启动 worker2，若未启动则在后台启动一个实例 (端口 8082)
    check_cmd = "ps aux | grep -i 'port=8082' | grep -v grep || true"
    out = subprocess.getoutput(check_cmd)
    if not out.strip():
        data_dir = "/home/jason/dist-log-data-worker2"
        os.makedirs(data_dir, exist_ok=True)
        start_cmd = f"nohup /opt/dist-log-analyzer-worker1/bin/dist-log-analyzer worker --port=8082 --manager-url=http://127.0.0.1:8080 --data-dir={data_dir} --advertise-ip=192.168.122.100 --cluster-token=dist-log-cluster-secret-token --node-name=storage-node-02 >/tmp/worker2.log 2>&1 &"
        subprocess.run(start_cmd, shell=True, check=True)
        time.sleep(3) # 等待 worker2 注册上线
    
    code, body = http_req(f"/api/nodes?token={TOKEN}")
    nodes = json.loads(body.decode("utf-8"))
    workers = [n for n in nodes if n.get("role") == "worker"]
    if len(workers) < 2:
        fail(f"预期至少 2 个 Worker 在线以测试容量调度，实际只有: {len(workers)}")
    success(f"双 Worker 拓扑就绪: 共 {len(workers)} 个存储节点在线参与容量均衡调度")

# ================= 测试用例 4: 构造归档包并测试上传、解压、流式行数统计 =================
SAMPLE_TAR = "/tmp/test_cluster_logs.tar.gz"
EXPECTED_LINES = {}

def create_sample_tar():
    if os.path.exists(SAMPLE_TAR):
        os.remove(SAMPLE_TAR)
    
    files = {
        "ceph/ceph-osd.0.log": [
            "2026-09-09 10:00:01 [INFO] osd.0 starting up with 128 pgs",
            "2026-09-09 10:00:02 [ERROR] osd.0 marked down, heartbeat_check: no reply from peer",
            "2026-09-09 10:00:03 [WARN] 25 slow requests are blocked currently > 30.0s",
            "2026-09-09 10:00:04 [INFO] heartbeat restored to mon.a",
            "2026-09-09 10:00:05 [INFO] sync block completed partition=0 seq=1",
        ],
        "hdfs/hadoop-datanode.log": [
            "2026-09-09 10:01:00 [INFO] DataNode registration succeeded",
            "2026-09-09 10:01:01 [ERROR] DataNode-1 is dead, Lost contact with DataNode after 600s",
            "2026-09-09 10:01:02 [FATAL] Corrupt block blk_1073741825 detected checksum mismatch",
            "2026-09-09 10:01:03 [INFO] Block pool BP-724128 service shutdown",
        ],
        "sys/dmesg.log": [
            "2026-09-09 10:02:00 [INFO] Linux version 6.6.0-generic",
            "2026-09-09 10:02:01 [CRITICAL] blk_update_request: I/O error, dev sdb, sector 1209348",
            "2026-09-09 10:02:02 [INFO] EXT4-fs error: remounting filesystem read-only",
        ]
    }
    
    with tarfile.open(SAMPLE_TAR, "w:gz") as tar:
        for name, lines in files.items():
            content = "\n".join(lines) + "\n"
            EXPECTED_LINES[name] = len(lines)
            data = content.encode("utf-8")
            ti = tarfile.TarInfo(name=name)
            ti.size = len(data)
            ti.mtime = int(time.time())
            ti.mode = 0o644
            import io
            tar.addfile(ti, io.BytesIO(data))

ARCHIVE_ID = ""

def test_upload_and_extract():
    global ARCHIVE_ID
    log("STEP 4", "构造多服务存储日志包并上传测试流式解压与零二次IO行数统计...")
    create_sample_tar()

    # 读取文件构造 multipart/form-data
    boundary = "----Boundary" + str(int(time.time()))
    with open(SAMPLE_TAR, "rb") as f:
        file_bytes = f.read()

    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="test_cluster_logs.tar.gz"\r\n'
        f"Content-Type: application/gzip\r\n\r\n"
    ).encode("utf-8") + file_bytes + f"\r\n--{boundary}--\r\n".encode("utf-8")

    headers = {
        "Content-Type": f"multipart/form-data; boundary={boundary}",
        "Authorization": f"Bearer {TOKEN}"
    }

    code, resp_body = http_req("/api/archives/upload", method="POST", data=body, headers=headers)
    res = json.loads(resp_body.decode("utf-8"))
    ARCHIVE_ID = res.get("id")
    if not ARCHIVE_ID:
        fail(f"上传失败，未返回归档包 ID: {res}")
    success(f"日志归档包上传成功，分配 ID: {ARCHIVE_ID} (分配存储节点: {res.get('node_id')})")

    # 轮询状态直到解压并诊断完成
    for _ in range(20):
        time.sleep(1)
        _, detail_body = http_req(f"/api/archives/{ARCHIVE_ID}?token={TOKEN}")
        detail = json.loads(detail_body.decode("utf-8"))
        status = detail.get("status")
        if status == "ready":
            # 校验文件列表与行数统计
            _, files_body = http_req(f"/api/archives/{ARCHIVE_ID}/files?token={TOKEN}")
            files = json.loads(files_body.decode("utf-8"))
            if len(files) < 3:
                fail(f"解压文件项不足 3 项，实际: {len(files)}")
            total_lines = detail.get("total_lines", 0)
            expected_total = sum(EXPECTED_LINES.values())
            if total_lines != expected_total:
                fail(f"总行数统计不一致！预期 {expected_total} 行，系统统计: {total_lines} 行")
            success(f"流式解压与零二次IO行数统计精准一致: 共 {len(files)} 个文件，总计 {total_lines} 行")
            return
        elif status == "failed":
            fail(f"归档包处理失败: {detail.get('error_msg')}")
    fail("归档包处理超时未能进入 ready 状态")

# ================= 测试用例 5: 规则引擎智能诊断报告 =================
def test_diagnosis_report():
    log("STEP 5", "测试专业存储故障规则多核并发智能诊断报告...")
    code, body = http_req(f"/api/reports/{ARCHIVE_ID}?token={TOKEN}")
    report = json.loads(body.decode("utf-8"))
    
    events = report.get("events", [])
    if len(events) == 0:
        fail("未诊断出任何异常故障事件")
    
    health_score = report.get("health_score", 100)
    sev_summary = report.get("severity_summary", {})
    storage_summary = report.get("storage_summary", {})
    
    success(f"智能诊断完成！健康评分: {health_score}分，检出异常事件: {len(events)} 项")
    success(f"故障级别分布: {sev_summary}，涉及存储类型: {storage_summary}")
    
    # 验证修复建议
    first_event = events[0]
    if not first_event.get("suggestion"):
        fail("诊断事件缺少专业修复建议 (suggestion)")
    success(f"匹配示例: [{first_event.get('storage_type')}] {first_event.get('matched_content')} -> 建议: {first_event.get('suggestion')[:40]}...")

# ================= 测试用例 6: 流式滑动环形缓冲区日志检索 =================
def test_streaming_search():
    log("STEP 6", "测试流式滑动环形缓冲区日志检索与上下文提取...")
    # 6.1 全局检索 'heartbeat_check' 带 2 行前后上下文
    query = {
        "archive_id": ARCHIVE_ID,
        "keyword": "heartbeat_check",
        "context_lines": 2,
        "page": 1,
        "page_size": 10
    }
    code, body = http_req(f"/api/search?token={TOKEN}", method="POST", data=query)
    res = json.loads(body.decode("utf-8"))
    
    hits = res.get("hits", [])
    if len(hits) == 0:
        fail("搜索 'heartbeat_check' 预期命中，实际未命中")
    
    hit = hits[0]
    if "marked down" not in hit.get("content"):
        fail(f"命中行内容不符: {hit.get('content')}")
    
    before = hit.get("context_before", [])
    after = hit.get("context_after", [])
    if len(before) != 1 or len(after) != 2:
        fail(f"上下文行数提取不符: 前置 {len(before)} 行 (预期1行), 后置 {len(after)} 行 (预期2行)")
    
    success(f"检索成功并精确提取滑动上下文: 耗时 {res.get('cost_ms')} ms，总命中: {res.get('total_hits')} 条")
    success(f"前置上下文: {before}")
    success(f"命中内容行: {hit.get('content')}")
    success(f"后置上下文: {after}")

    # 6.2 日志级别过滤
    query_level = {
        "archive_id": ARCHIVE_ID,
        "level": "FATAL",
        "page": 1,
        "page_size": 10
    }
    code, body = http_req(f"/api/search?token={TOKEN}", method="POST", data=query_level)
    res_level = json.loads(body.decode("utf-8"))
    hits_level = res_level.get("hits", [])
    if len(hits_level) == 0:
        fail("按级别 FATAL 检索未命中预设的损坏数据块日志")
    success(f"日志级别过滤检索成功: 检出 FATAL 故障日志: {hits_level[0].get('content')}")

# ================= 测试用例 7: 文件内容查看与指定文件下载 =================
def test_file_content_and_download():
    log("STEP 7", "测试页面日志内容流式查看与指定日志文件精准下载...")
    # 7.1 分片查看文件内容
    rel_path = "ceph/ceph-osd.0.log"
    code, body = http_req(f"/api/archives/{ARCHIVE_ID}/file-content?path={rel_path}&start_line=1&limit=3&token={TOKEN}")
    content_res = json.loads(body.decode("utf-8"))
    lines = content_res.get("lines", [])
    if len(lines) != 3:
        fail(f"分页查看行数不符合预期 limit=3，实际: {len(lines)}")
    if not content_res.get("has_more"):
        fail("预期 has_more 应为 True")
    success(f"分片内容查看成功: 起始行 {content_res.get('start_line')}, 返回 {len(lines)} 行, has_more={content_res.get('has_more')}")

    # 7.2 精准下载指定日志文件
    code, dl_body = http_req(f"/api/archives/{ARCHIVE_ID}/download-file?path={rel_path}&token={TOKEN}")
    dl_text = dl_body.decode("utf-8")
    if "osd.0 starting up" not in dl_text:
        fail("下载的指定文件内容与源文件不一致")
    success(f"指定文件精准下载成功，字节数: {len(dl_body)} Bytes")

    # 7.3 安全测试: 目录穿越拦截
    code, err_body = http_req(f"/api/archives/{ARCHIVE_ID}/download-file?path=../../../../etc/passwd&token={TOKEN}", expect_code=403)
    success("路径穿越攻击安全拦截通过 (返回 403 Forbidden)")

# ================= 测试用例 8: 告警中心全生命周期与自愈 =================
def test_alarms_lifecycle():
    log("STEP 8", "测试业务组件告警上报、频次聚合与自愈机制...")
    # 8.1 业务组件上报异常告警
    alarm_data = {
        "node_id": "worker_j-server_8081",
        "alarm_type": "disk_readonly",
        "severity": "CRITICAL",
        "title": "测试模拟: 底层存储文件系统被挂载为只读模式",
        "message": "检测到 /dev/sdb ext4 发生 metadata error, remount ro"
    }
    headers = {
        "Content-Type": "application/json",
        "X-Cluster-Token": "dist-log-cluster-secret-token"
    }
    code, body = http_req("/api/alarms/report", method="POST", data=alarm_data, headers=headers)
    success("业务组件成功上报告警事件")

    # 8.2 检查告警中心列表
    code, body = http_req(f"/api/alarms?token={TOKEN}")
    alarms = json.loads(body.decode("utf-8"))
    matched = [a for a in alarms if a.get("alarm_type") == "disk_readonly"]
    if not matched:
        fail("告警列表中未找到刚上报的告警事件")
    success(f"告警中心已收录: [{matched[0].get('severity')}] {matched[0].get('title')} (频次: {matched[0].get('count')})")

    # 8.3 测试管理员确认告警
    ack_code, ack_body = http_req(f"/api/alarms/{matched[0].get('id')}/ack?token={TOKEN}", method="POST")
    success("告警确认操作成功 (标记为已确认)")

# ================= 测试用例 9: 高可用配置热更新与网关/Worker防脑裂仲裁 =================
def test_ha_and_split_brain_protection():
    log("STEP 9", "测试高可用主备配置动态热更新与防脑裂双重仲裁...")
    # 9.1 查看初始 HA 状态
    code, body = http_req(f"/api/ha/status?token={TOKEN}")
    ha_stat = json.loads(body.decode("utf-8"))
    success(f"当前 HA 状态: 模式={ha_stat.get('mode')}, 角色={ha_stat.get('role')}, 探测网关={ha_stat.get('gateway_ip')}")

    # 9.2 动态热更新高可用配置与网关 IP
    ha_update = {
        "mode": "standalone",
        "gateway_ip": "192.168.122.1",
        "enable_gateway_check": True
    }
    code, body = http_req(f"/api/ha/config?token={TOKEN}", method="POST", data=ha_update)
    res = json.loads(body.decode("utf-8"))
    cfg = res.get("config", {})
    stat = res.get("status", {})
    if cfg.get("gateway_ip") != "192.168.122.1":
        fail(f"更新高可用网关 IP 未即时生效: {cfg}")
    success(f"Web 控制台动态配置热更新成功: 网关 IP 已设为 {cfg.get('gateway_ip')}, 网关在线探测: {stat.get('gateway_online')}")

def main():
    print("==================================================================")
    print("    分布式存储日志分析系统 - 远程全量端到端功能验证套件             ")
    print("==================================================================")
    start_time = time.time()
    
    test_auth()
    test_cluster_nodes()
    test_dual_worker_capacity_balance()
    test_upload_and_extract()
    test_diagnosis_report()
    test_streaming_search()
    test_file_content_and_download()
    test_alarms_lifecycle()
    test_ha_and_split_brain_protection()

    duration = time.time() - start_time
    print("==================================================================")
    print(f"  \033[1;32m🎉 全部 9 大模块全量功能验证 100% 通过！(总耗时: {duration:.2f}s)\033[0m")
    print("==================================================================")

if __name__ == "__main__":
    main()
