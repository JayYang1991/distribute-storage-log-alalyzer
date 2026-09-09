// 分布式存储日志分析系统 前端应用核心逻辑 (原生纯JS，零外部网络依赖)

const app = {
  token: localStorage.getItem("token") || "",
  currentUser: null,
  activeTab: "dashboard",
  archives: [],
  nodes: [],
  users: [],
  rules: [],
  currentViewingArchiveID: null,

  init() {
    this.bindEvents();
    if (this.token) {
      this.fetchMe();
    } else {
      this.openModal("modal-login");
    }
  },

  bindEvents() {
    // 导航切换
    document.querySelectorAll(".nav-item").forEach(item => {
      item.addEventListener("click", () => {
        const tab = item.dataset.tab;
        this.switchTab(tab);
      });
    });

    // 登出
    document.getElementById("btn-logout").addEventListener("click", () => this.doLogout());

    // 刷新按钮
    document.getElementById("btn-refresh").addEventListener("click", () => this.refreshData());

    // 快速上传按钮
    document.getElementById("btn-quick-upload").addEventListener("click", () => {
      this.switchTab("archives");
      document.getElementById("file-input").click();
    });

    // 拖拽上传
    const dropzone = document.getElementById("archive-dropzone");
    const fileInput = document.getElementById("file-input");

    dropzone.addEventListener("click", () => fileInput.click());
    dropzone.addEventListener("dragover", e => {
      e.preventDefault();
      dropzone.classList.add("dragover");
    });
    dropzone.addEventListener("dragleave", () => dropzone.classList.remove("dragover"));
    dropzone.addEventListener("drop", e => {
      e.preventDefault();
      dropzone.classList.remove("dragover");
      if (e.dataTransfer.files.length > 0) {
        this.uploadFile(e.dataTransfer.files[0]);
      }
    });
    fileInput.addEventListener("change", e => {
      if (e.target.files.length > 0) {
        this.uploadFile(e.target.files[0]);
      }
    });

    // 定时轮询（每 10 秒刷新节点与任务）
    setInterval(() => {
      if (this.token && !document.hidden) {
        this.silentRefresh();
      }
    }, 10000);
  },

  // ================= 认证与用户 =================

  async doLogin() {
    const u = document.getElementById("login-username").value;
    const p = document.getElementById("login-password").value;
    const errBox = document.getElementById("login-error");
    errBox.style.display = "none";

    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: u, password: p }),
      });
      if (!res.ok) {
        const txt = await res.text();
        throw new Error(txt || "登录失败");
      }
      const data = await res.json();
      this.token = data.token;
      this.currentUser = data.user;
      localStorage.setItem("token", this.token);
      this.closeModal("modal-login");
      this.updateUserUI();
      this.refreshData();
    } catch (err) {
      errBox.innerText = err.message;
      errBox.style.display = "block";
    }
  },

  async fetchMe() {
    try {
      const res = await this.api("/api/auth/me");
      if (res.ok) {
        this.currentUser = await res.json();
        this.updateUserUI();
        this.refreshData();
      } else {
        this.doLogout();
      }
    } catch (e) {
      this.doLogout();
    }
  },

  doLogout() {
    this.api("/api/auth/logout", "POST");
    this.token = "";
    this.currentUser = null;
    localStorage.removeItem("token");
    this.openModal("modal-login");
  },

  updateUserUI() {
    if (!this.currentUser) return;
    document.getElementById("user-name").innerText = this.currentUser.username;
    document.getElementById("user-role").innerText = this.currentUser.role === "admin" ? "系统管理员" : "业务用户";
    document.getElementById("user-avatar").innerText = this.currentUser.username.substring(0, 1).toUpperCase();

    // 权限控制：普通用户隐藏多用户管理与规则修改
    const userNav = document.getElementById("nav-users");
    const adminRuleActions = document.getElementById("admin-rule-actions");
    if (this.currentUser.role !== "admin") {
      userNav.style.display = "none";
      if (adminRuleActions) adminRuleActions.style.display = "none";
    } else {
      userNav.style.display = "flex";
      if (adminRuleActions) adminRuleActions.style.display = "flex";
    }
  },

  // ================= 导航切换 =================

  switchTab(tabName) {
    this.activeTab = tabName;
    document.querySelectorAll(".nav-item").forEach(item => {
      item.classList.toggle("active", item.dataset.tab === tabName);
    });

    const titles = {
      dashboard: ["概览仪表盘", "实时监控分布式存储计算节点状态与日志分析全局指标"],
      nodes: ["集群节点与安装", "管理分布式计算节点与一键 SSH 远程部署业务组件"],
      users: ["用户与存储空间", "多租户权限控制与用户独立存储空间沙箱配额管理"],
      archives: ["日志归档与文件", "常用格式压缩包上传、解包文件树目录与日志在线查看"],
      search: ["日志全局检索", "毫秒级正则与全文检索引擎，支持行级定位与上下文展开"],
      rules: ["故障规则与诊断", "配置存储故障模式规则库，自动诊断与自愈排查指导"],
      alarms: ["实时告警中心", "业务计算节点异常自检、硬件磁盘故障、节点失联与系统告警全生命周期监控"],
    };

    if (titles[tabName]) {
      document.getElementById("page-title").innerText = titles[tabName][0];
      document.getElementById("page-desc").innerText = titles[tabName][1];
    }

    document.querySelectorAll(".tab-pane").forEach(pane => {
      pane.style.display = pane.id === `pane-${tabName}` ? "block" : "none";
    });

    // 切换到对应 tab 触发加载
    if (tabName === "search") {
      this.populateSearchArchiveSelect();
    } else if (tabName === "alarms") {
      this.fetchAlarms();
    }
  },

  // ================= 数据刷新 =================

  async refreshData() {
    await Promise.all([
      this.loadNodes(),
      this.loadArchives(),
      this.loadRules(),
      this.fetchHAStatus(),
      this.fetchAlarmSummary(),
      this.currentUser && this.currentUser.role === "admin" ? this.loadUsers() : Promise.resolve(),
    ]);
    if (this.activeTab === "alarms") {
      await this.fetchAlarms();
    }
    this.updateDashboardStats();
  },

  async silentRefresh() {
    await Promise.all([
      this.loadNodes(true),
      this.loadArchives(true),
      this.fetchHAStatus(),
      this.fetchAlarmSummary(),
    ]);
    if (this.activeTab === "alarms") {
      await this.fetchAlarms();
    }
    this.updateDashboardStats();
  },

  // ================= 高可用 (HA) 状态管理 =================

  async fetchHAStatus() {
    try {
      const res = await this.api("/api/ha/status");
      if (!res.ok) return;
      const ha = await res.json();
      this.haStatus = ha;
      this.renderHAStatus(ha);
    } catch (e) {
      console.warn("获取 HA 状态失败", e);
    }
  },

  renderHAStatus(ha) {
    const topText = document.getElementById("ha-top-text");
    const roleBadge = document.getElementById("ha-role-badge");
    const switchoverBtn = document.getElementById("btn-ha-switchover");
    const grid = document.getElementById("ha-info-grid");

    let roleLabel = "单节点 (Standalone)";
    let badgeClass = "badge-info";
    let isAct = ha.role === "active";

    if (ha.mode === "standalone") {
      roleLabel = "单节点独立模式";
      badgeClass = "badge-info";
      if (topText) topText.innerHTML = "模式: <strong>单节点独立</strong>";
      if (switchoverBtn) switchoverBtn.style.display = "none";
    } else {
      if (isAct) {
        roleLabel = `主节点 (Active - ${ha.mode === 'primary' ? '首选主' : '接管主'})`;
        badgeClass = "badge-success";
        if (topText) topText.innerHTML = `HA: <strong style="color:#10b981;">主管理节点 (Active)</strong>`;
      } else {
        roleLabel = "备节点 (Standby - 热备待命)";
        badgeClass = "badge-warning";
        if (topText) topText.innerHTML = `HA: <strong style="color:#f59e0b;">备管理节点 (Standby - 实时同步)</strong>`;
      }
      if (switchoverBtn) {
        switchoverBtn.style.display = (ha.peer_online && this.currentUser && this.currentUser.role === 'admin') ? "inline-block" : "none";
      }
    }

    if (roleBadge) {
      roleBadge.innerText = roleLabel;
      roleBadge.className = `badge ${badgeClass}`;
    }

    if (grid) {
      const peerStatus = ha.mode === "standalone" 
        ? '<span style="color:var(--text-muted);">未配置 (单机运行)</span>'
        : ha.peer_online 
        ? `<span style="color:#10b981;">● 在线 (往返延迟: ${ha.peer_latency_ms}ms)</span>`
        : '<span style="color:#ef4444;">● 离线 / 未连接</span>';

      const syncInfo = ha.mode === "standalone"
        ? '<span style="color:var(--text-muted);">-</span>'
        : ha.role === "active"
        ? '<span style="color:#10b981;">主节点数据源 (持续对外输出快照)</span>'
        : ha.sync_status === "synced"
        ? `<span style="color:#10b981;">已同步 (最后: ${ha.last_sync_time ? new Date(ha.last_sync_time).toLocaleTimeString() : '-'})</span>`
        : `<span style="color:#f59e0b;">${ha.sync_status || '待命'}</span>`;

      const vipInfo = ha.vip 
        ? `${this.escape(ha.vip)} ${ha.vip_active ? '<strong style="color:#10b981;">[本机持有]</strong>' : '<span style="color:var(--text-muted);">[未持有]</span>'}`
        : '<span style="color:var(--text-muted);">未启用</span>';

      const gwInfo = ha.gateway_online
        ? `<span style="color:#10b981;">● 正常 (${ha.gateway_ip || '自动探测'})</span>`
        : `<span style="color:#ef4444;">● 异常/不可达 (${ha.gateway_ip || '网络孤岛'})</span>`;

      const quorumInfo = (ha.worker_quorum_total === 0)
        ? '<span style="color:var(--text-muted);">集群无 Worker (网关守护)</span>'
        : ha.quorum_passed
        ? `<span style="color:#10b981;">● 多数派通过 (${ha.worker_quorum_online}/${ha.worker_quorum_total} 在线)</span>`
        : `<span style="color:#ef4444;">● 未达过半数 (${ha.worker_quorum_online}/${ha.worker_quorum_total} 孤立)</span>`;

      const splitBrainAlert = ha.split_brain_blocked
        ? `<div style="grid-column: 1 / -1; background: rgba(239,68,68,0.15); border: 1px solid #ef4444; border-radius: var(--radius-sm); padding: 10px 14px; color: #f87171; font-size: 13px;">
             <strong>⚠️ 防脑裂机制已触发并保护中：</strong>${this.escape(ha.blocked_reason || '心跳虽丢失，但本机未达到法定多数派或网关失联，已严格阻止盲目晋升以防双主！')}
           </div>`
        : '';

      grid.innerHTML = `
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">部署模式 / 本机角色</div>
          <div style="font-weight: 600; color: var(--text-primary); font-size: 14px;">${(ha.mode || 'standalone').toUpperCase()} / <span class="badge ${badgeClass}">${(ha.role || 'active').toUpperCase()}</span></div>
        </div>
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">对端对等节点状态</div>
          <div style="font-size: 13px;">${peerStatus}</div>
          <div style="font-size: 11px; color: var(--text-muted); margin-top: 2px;">${this.escape(ha.peer_url || '无')}</div>
        </div>
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">元数据与配置同步</div>
          <div style="font-size: 13px;">${syncInfo}</div>
          <div style="font-size: 11px; color: var(--text-muted); margin-top: 2px;">同步流量: ${((ha.last_sync_bytes || 0) / 1024).toFixed(1)} KB</div>
        </div>
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">虚拟高可用 IP (VIP)</div>
          <div style="font-size: 13px;">${vipInfo}</div>
          <div style="font-size: 11px; color: var(--text-muted); margin-top: 2px;">自动故障切换累计: ${ha.failover_count || 0} 次</div>
        </div>
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">🛡️ 网关连通性自检</div>
          <div style="font-size: 13px;">${gwInfo}</div>
          <div style="font-size: 11px; color: var(--text-muted); margin-top: 2px;">防孤岛误抢主屏障</div>
        </div>
        <div style="background: rgba(15,23,42,0.4); padding: 12px; border-radius: var(--radius-sm); border: 1px solid var(--border-color);">
          <div style="font-size: 11px; color: var(--text-muted); margin-bottom: 4px;">⚖️ Worker 多数派仲裁</div>
          <div style="font-size: 13px;">${quorumInfo}</div>
          <div style="font-size: 11px; color: var(--text-muted); margin-top: 2px;">反向法定人数仲裁</div>
        </div>
        ${splitBrainAlert}
      `;
    }
  },

  async doHASwitchover() {
    if (!confirm("⚠️ 确定要执行管理组件主备平滑倒换吗？\n当前主节点将降级为备用待命节点，对端备节点将晋升为主节点接管全部读写调度。")) {
      return;
    }
    const btn = document.getElementById("btn-ha-switchover");
    btn.disabled = true;
    btn.innerText = "倒换中...";
    try {
      const res = await this.api("/api/ha/switchover", "POST");
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      alert("✅ " + data.message);
      await this.fetchHAStatus();
    } catch (e) {
      alert("❌ 主备倒换失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.innerText = "⚡ 一键主备平滑倒换";
    }
  },

  async openHAConfigModal() {
    try {
      const res = await this.api("/api/ha/config");
      let cfg = {};
      if (res.ok) {
        cfg = await res.json();
      }

      document.getElementById("ha-cfg-mode").value = cfg.ha_mode || "standalone";
      document.getElementById("ha-cfg-peer-url").value = cfg.peer_url || "";
      document.getElementById("ha-cfg-gateway-ip").value = cfg.gateway_ip || "";
      document.getElementById("ha-cfg-vip").value = cfg.vip || "";
      document.getElementById("ha-cfg-vip-interface").value = cfg.vip_interface || "";
      document.getElementById("ha-cfg-enable-gateway").checked = (cfg.enable_gateway_check !== false);
      document.getElementById("ha-cfg-enable-quorum").checked = (cfg.enable_worker_quorum !== false);
      document.getElementById("ha-cfg-heartbeat").value = cfg.heartbeat_interval_sec || 2;
      document.getElementById("ha-cfg-failover").value = cfg.failover_timeout_sec || 6;
      document.getElementById("ha-cfg-sync").value = cfg.sync_interval_sec || 5;

      this.onHAModeChange();
      this.openModal("modal-ha-config");
    } catch (e) {
      alert("获取高可用与网络配置失败: " + e.message);
    }
  },

  onHAModeChange() {
    const mode = document.getElementById("ha-cfg-mode").value;
    const peerInput = document.getElementById("ha-cfg-peer-url");
    if (mode === "standalone") {
      peerInput.placeholder = "单节点独立模式无需填写对端地址 (可留空)";
    } else if (mode === "primary") {
      peerInput.placeholder = "例如：http://192.168.1.11:8080 (对端备节点地址)";
    } else {
      peerInput.placeholder = "例如：http://192.168.1.10:8080 (对端主节点地址)";
    }
  },

  async saveHAConfig() {
    const btn = document.getElementById("btn-save-ha-cfg");
    btn.disabled = true;
    btn.innerText = "正在保存并热生效...";

    const payload = {
      ha_mode: document.getElementById("ha-cfg-mode").value,
      peer_url: document.getElementById("ha-cfg-peer-url").value.trim(),
      gateway_ip: document.getElementById("ha-cfg-gateway-ip").value.trim(),
      vip: document.getElementById("ha-cfg-vip").value.trim(),
      vip_interface: document.getElementById("ha-cfg-vip-interface").value.trim(),
      enable_gateway_check: document.getElementById("ha-cfg-enable-gateway").checked,
      enable_worker_quorum: document.getElementById("ha-cfg-enable-quorum").checked,
      heartbeat_interval_sec: parseInt(document.getElementById("ha-cfg-heartbeat").value, 10) || 2,
      failover_timeout_sec: parseInt(document.getElementById("ha-cfg-failover").value, 10) || 6,
      sync_interval_sec: parseInt(document.getElementById("ha-cfg-sync").value, 10) || 5,
    };

    try {
      const res = await this.api("/api/ha/config", "POST", payload);
      if (!res.ok) {
        const errText = await res.text();
        throw new Error(errText);
      }
      const data = await res.json();
      alert("✅ " + (data.message || "高可用与网络配置已保存并立即生效！"));
      this.closeModal("modal-ha-config");
      await this.fetchHAStatus();
    } catch (e) {
      alert("❌ 保存高可用配置失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.innerText = "💾 保存配置并立即生效";
    }
  },

  async api(url, method = "GET", body = null) {
    const headers = {};
    if (this.token) {
      headers["Authorization"] = `Bearer ${this.token}`;
    }
    if (body && !(body instanceof FormData)) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    }
    return fetch(url, { method, headers, body });
  },

  // ================= 节点管理 =================

  async loadNodes(silent = false) {
    try {
      const res = await this.api("/api/nodes");
      if (res.ok) {
        this.nodes = await res.json();
        this.renderNodes();
        this.populateUploadNodes();
      }
    } catch (e) {
      if (!silent) console.error("加载节点失败", e);
    }
  },

  populateUploadNodes() {
    const sel = document.getElementById("upload-target-node");
    if (!sel) return;
    const currentVal = sel.value;
    sel.innerHTML = '<option value="manager_primary">管理节点本地存储</option>';
    this.nodes.forEach(n => {
      if (n.role === "worker" && n.status === "online") {
        const diskInfo = n.disk_device ? ` [磁盘: ${n.disk_device}]` : '';
        const freeGB = n.resource && n.resource.disk_free_mb ? ` (可用: ${(n.resource.disk_free_mb/1024).toFixed(1)}GB)` : '';
        sel.innerHTML += `<option value="${n.id}">${this.escape(n.name)} - ${n.ip}:${n.port}${diskInfo}${freeGB}</option>`;
      }
    });
    if (currentVal) {
      sel.value = currentVal;
    }
  },

  renderNodes() {
    const tbody = document.querySelector("#table-nodes tbody");
    if (!tbody) return;
    tbody.innerHTML = "";

    this.nodes.forEach(n => {
      const tr = document.createElement("tr");
      const isOnline = n.status === "online";
      const statusBadge = isOnline
        ? '<span class="badge badge-success">在线</span>'
        : n.status === "installing"
        ? '<span class="badge badge-warning">正在安装</span>'
        : '<span class="badge badge-danger">离线</span>';

      const memStr = n.resource ? `${n.resource.mem_used_mb || 0} / ${n.resource.mem_total_mb || 0} MB` : "-";
      const diskStr = n.resource ? `${(n.resource.disk_free_mb / 1024).toFixed(1)} GB` : "-";
      const lastHb = n.last_heartbeat ? new Date(n.last_heartbeat).toLocaleTimeString() : "-";
      const diskBadge = n.disk_device ? `<span style="font-size: 11px; color: var(--primary); display: block;">💾 ${this.escape(n.disk_device)} (${n.fs_type || 'ext4'})</span>` : '';

      tr.innerHTML = `
        <td><strong>${this.escape(n.name)}</strong>${diskBadge}</td>
        <td><span class="badge ${n.role === "manager" ? "badge-info" : "badge-muted"}">${n.role}</span></td>
        <td><code>${this.escape(n.ip)}:${n.port}</code></td>
        <td>${statusBadge}</td>
        <td>${n.active_tasks || 0}</td>
        <td>${memStr}</td>
        <td>${diskStr}</td>
        <td>${lastHb}</td>
        <td>
          ${n.role !== "manager" && this.currentUser?.role === "admin" ? `
            <button class="btn btn-danger btn-sm" onclick="app.deleteNode('${n.id}')">移除</button>
          ` : '<span style="color: var(--text-dim); font-size: 11px;">核心管理节点</span>'}
        </td>
      `;
      tbody.appendChild(tr);
    });
  },

  openDeployModal() {
    document.getElementById("deploy-terminal-box").style.display = "none";
    document.getElementById("btn-deploy-submit").disabled = false;
    this.openModal("modal-deploy");
  },

  async detectDisks() {
    const host = document.getElementById("deploy-host").value.trim();
    const port = parseInt(document.getElementById("deploy-port").value, 10) || 22;
    const username = document.getElementById("deploy-user").value.trim();
    const password = document.getElementById("deploy-pass").value;

    if (!host || !username) {
      alert("请先填写目标主机 IP 与 SSH 登录凭据！");
      return;
    }

    const btn = document.getElementById("btn-detect-disks");
    const hint = document.getElementById("deploy-disk-hint");
    btn.disabled = true;
    btn.innerText = "探测中...";
    if (hint) hint.style.display = "none";

    try {
      const res = await this.api("/api/nodes/detect-disks", "POST", { host, port, username, password });
      if (!res.ok) throw new Error(await res.text());
      const disks = await res.json();
      this._lastDetectedDisks = disks || [];
      const select = document.getElementById("deploy-disk-select");
      const diskInput = document.getElementById("deploy-disk-input");

      select.innerHTML = '<option value="">-- 请选择检测到的物理硬盘 --</option>';

      if (!disks || disks.length === 0) {
        select.innerHTML = '<option value="">未检测到物理磁盘，请检查目标主机权限</option>';
        if (hint) {
          hint.style.display = "block";
          hint.style.background = "rgba(245, 158, 11, 0.15)";
          hint.style.border = "1px solid #f59e0b";
          hint.style.color = "#fbbf24";
          hint.innerText = "⚠️ 未检测到可用的磁盘设备，请确认目标主机是否有外挂独立物理硬盘。";
        }
      } else {
        let firstSafeDisk = null;
        disks.forEach(d => {
          const modelInfo = d.model ? ` [${d.model}]` : '';
          if (d.can_format) {
            if (!firstSafeDisk) firstSafeDisk = d;
            select.innerHTML += `<option value="${d.path}">✅ [安全推荐: 裸盘] ${d.path} (${d.size})${modelInfo} - 无文件系统，安全可用</option>`;
          } else {
            select.innerHTML += `<option value="${d.path}" disabled style="color: #94a3b8; background: rgba(30,41,59,0.8);">🚫 [防呆锁定] ${d.path} (${d.size})${modelInfo} - ${d.status_text || '已有文件系统'}</option>`;
          }
        });

        if (firstSafeDisk) {
          // 自动选中第一个未格式化纯净裸盘
          select.value = firstSafeDisk.path;
          diskInput.value = firstSafeDisk.path;
          if (hint) {
            hint.style.display = "block";
            hint.style.background = "rgba(16, 185, 129, 0.15)";
            hint.style.border = "1px solid #10b981";
            hint.style.color = "#34d399";
            hint.innerHTML = `<strong>✅ 智能防呆已就绪：</strong>自动优选未格式化的纯净裸物理盘 <code>${firstSafeDisk.path}</code> (${firstSafeDisk.size})。已有文件系统或系统分区的磁盘已被自动锁定，避免误格式化破坏数据。`;
          }
        } else {
          // 全部盘都已有文件系统
          diskInput.value = "";
          if (hint) {
            hint.style.display = "block";
            hint.style.background = "rgba(239, 68, 68, 0.15)";
            hint.style.border = "1px solid #ef4444";
            hint.style.color = "#f87171";
            hint.innerHTML = `<strong>⚠️ 防呆保护告警：</strong>目标主机探测到的所有磁盘均已有文件系统或系统分区，已全部防呆锁定禁止格式化！如确需使用，请为目标主机挂载全新物理裸盘，或取消勾选下方的“自动格式化”。`;
          }
        }
      }
    } catch (e) {
      alert("探测目标主机磁盘失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.innerText = "🔍 探测目标磁盘";
    }
  },

  onDiskSelectChange() {
    const val = document.getElementById("deploy-disk-select").value;
    if (val) {
      document.getElementById("deploy-disk-input").value = val;
    }
  },

  async submitDeploy() {
    const host = document.getElementById("deploy-host").value.trim();
    const port = parseInt(document.getElementById("deploy-port").value, 10);
    const username = document.getElementById("deploy-user").value.trim();
    const password = document.getElementById("deploy-pass").value;
    const nodeName = document.getElementById("deploy-name").value.trim();

    const diskDevice = document.getElementById("deploy-disk-input").value.trim();
    const fsType = document.getElementById("deploy-fstype").value;
    const mountPoint = document.getElementById("deploy-mountpoint").value.trim();
    const formatDisk = document.getElementById("deploy-format-disk").checked;

    // 前端防呆校验：如果勾选了格式化，且选中的磁盘在探测结果中标记为不可格式化
    if (formatDisk && diskDevice && this._lastDetectedDisks && this._lastDetectedDisks.length > 0) {
      const matchDisk = this._lastDetectedDisks.find(d => d.path === diskDevice || d.name === diskDevice);
      if (matchDisk && !matchDisk.can_format) {
        alert(`【安全防呆拦截】目标磁盘 ${diskDevice} 检测到包含已有文件系统或系统关键分区（${matchDisk.status_text}）！\n为防止重要数据丢失或系统崩溃，系统拒绝执行格式化操作。\n请更换为未格式化的裸盘，或取消勾选“自动格式化该硬盘”。`);
        return;
      }
    }

    const termBox = document.getElementById("deploy-terminal-box");
    const term = document.getElementById("deploy-terminal");
    const btn = document.getElementById("btn-deploy-submit");

    termBox.style.display = "block";
    term.innerText = `[1/4] 准备向目标 ${host}:${port} 发起 SSH 远程一键部署...\n` +
      (diskDevice ? `[磁盘配置] 目标硬盘: ${diskDevice}, 文件系统: ${fsType}, 格式化: ${formatDisk}\n` : '') +
      `[2/4] 正在传输安装包并执行配置，请稍候...\n`;
    btn.disabled = true;

    try {
      const res = await this.api("/api/nodes/deploy", "POST", {
        host,
        port,
        username,
        password,
        node_name: nodeName,
        disk_device: diskDevice,
        fs_type: fsType,
        mount_point: mountPoint,
        format_disk: formatDisk,
      });
      if (!res.ok) {
        const err = await res.text();
        throw new Error(err);
      }
      term.innerText += `[3/4] 远程磁盘挂载与服务启动执行成功，等待 Worker 注册上线...\n`;
      
      setTimeout(() => this.loadNodes(), 2500);
      setTimeout(() => {
        term.innerText += `[4/4] 部署完成！请关闭窗口查看节点状态与硬盘信息。\n`;
        btn.disabled = false;
        this.loadNodes();
      }, 5000);
    } catch (e) {
      term.innerText += `❌ 安装失败: ${e.message}\n`;
      btn.disabled = false;
    }
  },

  async deleteNode(id) {
    if (!confirm("确定要从集群中移除该业务节点吗？")) return;
    await this.api(`/api/nodes/${id}`, "DELETE");
    this.loadNodes();
  },

  openAgentScriptModal() {
    const host = window.location.host;
    const proto = window.location.protocol;
    const cmd = `curl -sSL ${proto}//${host}/api/agent/install.sh | bash`;
    document.getElementById("agent-script-cmd").innerText = cmd;
    this.openModal("modal-agent-script");
  },

  copyAgentCmd() {
    const text = document.getElementById("agent-script-cmd").innerText;
    navigator.clipboard.writeText(text).then(() => {
      alert("离线一键安装接入命令已复制到剪贴板！可在远程机器终端粘贴运行。");
    });
  },

  // ================= 用户与存储空间 =================

  async loadUsers() {
    try {
      const res = await this.api("/api/users");
      if (res.ok) {
        this.users = await res.json();
        this.renderUsers();
      }
    } catch (e) {
      console.error(e);
    }
  },

  renderUsers() {
    const tbody = document.querySelector("#table-users tbody");
    if (!tbody) return;
    tbody.innerHTML = "";

    this.users.forEach(u => {
      const tr = document.createElement("tr");
      const usedMB = (u.used_storage_bytes / (1024 * 1024)).toFixed(1);
      const quotaMB = u.space_quota_bytes > 0 ? (u.space_quota_bytes / (1024 * 1024)).toFixed(0) : "不限";
      let percent = 0;
      if (u.space_quota_bytes > 0) {
        percent = Math.min(100, Math.round((u.used_storage_bytes / u.space_quota_bytes) * 100));
      }

      tr.innerHTML = `
        <td><strong>${this.escape(u.username)}</strong></td>
        <td><span class="badge ${u.role === "admin" ? "badge-info" : "badge-muted"}">${u.role}</span></td>
        <td><span class="badge badge-success">${u.status}</span></td>
        <td>${usedMB} MB</td>
        <td>${quotaMB === "不限" ? "不限" : quotaMB + " MB"}</td>
        <td style="width: 140px;">
          <div style="font-size: 11px; margin-bottom: 2px;">${percent}%</div>
          <div style="height: 6px; background: #334155; border-radius: 3px; overflow: hidden;">
            <div style="width: ${percent}%; height: 100%; background: ${percent > 85 ? "var(--danger)" : "var(--primary)"};"></div>
          </div>
        </td>
        <td>${new Date(u.created_at).toLocaleDateString()}</td>
        <td>
          ${u.username !== "admin" ? `
            <button class="btn btn-danger btn-sm" onclick="app.deleteUser('${u.username}')">删除空间</button>
          ` : '<span style="color: var(--text-dim); font-size: 11px;">默认管理员</span>'}
        </td>
      `;
      tbody.appendChild(tr);
    });
  },

  openCreateUserModal() {
    document.getElementById("user-username").value = "";
    document.getElementById("user-password").value = "";
    this.openModal("modal-user");
  },

  async submitUser() {
    const u = document.getElementById("user-username").value.trim();
    const p = document.getElementById("user-password").value;
    const r = document.getElementById("user-role-select").value;
    const q = parseInt(document.getElementById("user-quota-select").value, 10);

    try {
      const res = await this.api("/api/users", "POST", {
        username: u,
        password: p,
        role: r,
        space_quota_bytes: q,
      });
      if (!res.ok) {
        const err = await res.text();
        throw new Error(err);
      }
      this.closeModal("modal-user");
      this.loadUsers();
    } catch (e) {
      alert("创建用户失败: " + e.message);
    }
  },

  async deleteUser(username) {
    if (!confirm(`确定要彻底删除用户 ${username} 及其专属存储空间中的所有日志吗？`)) return;
    await this.api(`/api/users/${username}`, "DELETE");
    this.loadUsers();
  },

  // ================= 日志上传与归档 =================

  async loadArchives(silent = false) {
    try {
      const res = await this.api("/api/archives");
      if (res.ok) {
        this.archives = await res.json();
        this.renderArchives();
      }
    } catch (e) {
      if (!silent) console.error(e);
    }
  },

  renderArchives() {
    const renderTable = (tbodyId, isRecentOnly) => {
      const tbody = document.querySelector(tbodyId);
      if (!tbody) return;
      tbody.innerHTML = "";

      const list = isRecentOnly ? this.archives.slice(0, 5) : this.archives;
      if (list.length === 0) {
        tbody.innerHTML = `<tr><td colspan="9" style="text-align: center; color: var(--text-dim); padding: 24px;">暂无日志归档包，请点击上方“上传日志包”按钮开始分析</td></tr>`;
        return;
      }

      list.forEach(a => {
        const tr = document.createElement("tr");
        const sizeMB = (a.size / (1024 * 1024)).toFixed(2);
        const isReady = a.status === "ready";
        const statusBadge = isReady
          ? '<span class="badge badge-success">分析就绪</span>'
          : a.status === "failed"
          ? `<span class="badge badge-danger" title="${this.escape(a.error_msg || '')}">失败</span>`
          : '<span class="badge badge-warning">解包诊断中</span>';

        const storageNodeBadge = a.storage_node_name
          ? `<span class="badge badge-info" title="${this.escape(a.extract_path || '')}">💾 ${this.escape(a.storage_node_name)}</span>`
          : (a.assigned_worker ? `<span class="badge badge-info">${this.escape(a.assigned_worker)}</span>` : '<span class="badge badge-muted">管理节点本地</span>');

        if (isRecentOnly) {
          tr.innerHTML = `
            <td><strong>${this.escape(a.filename)}</strong></td>
            <td>${this.escape(a.username)}</td>
            <td>${sizeMB} MB</td>
            <td>${a.file_count || 0}</td>
            <td>${statusBadge}</td>
            <td>${new Date(a.upload_time).toLocaleString()}</td>
            <td>
              ${isReady ? `
                <button class="btn btn-secondary btn-sm" onclick="app.viewFiles('${a.id}')">浏览</button>
                <button class="btn btn-primary btn-sm" onclick="app.viewDiagnosis('${a.id}')">诊断报告</button>
              ` : '-'}
            </td>
          `;
        } else {
          tr.innerHTML = `
            <td><strong>${this.escape(a.filename)}</strong></td>
            <td><code>.${a.format}</code></td>
            <td>${sizeMB} MB</td>
            <td>${a.file_count || 0}</td>
            <td>${a.total_lines ? a.total_lines.toLocaleString() : 0}</td>
            <td>${storageNodeBadge}</td>
            <td>${statusBadge}</td>
            <td>${new Date(a.upload_time).toLocaleString()}</td>
            <td>
              ${isReady ? `
                <button class="btn btn-secondary btn-sm" onclick="app.viewFiles('${a.id}')">📁 浏览</button>
                <button class="btn btn-primary btn-sm" onclick="app.viewDiagnosis('${a.id}')">🛡️ 报告</button>
              ` : ''}
              <button class="btn btn-danger btn-sm" onclick="app.deleteArchive('${a.id}')">删除</button>
            </td>
          `;
        }
        tbody.appendChild(tr);
      });
    };

    renderTable("#table-recent-archives tbody", true);
    renderTable("#table-all-archives tbody", false);
  },

  async uploadFile(file) {
    const pBox = document.getElementById("upload-progress-box");
    const pBar = document.getElementById("upload-progress-bar");
    const pPercent = document.getElementById("upload-percent");
    const pName = document.getElementById("upload-filename");

    const targetNodeSelect = document.getElementById("upload-target-node");
    const targetNodeID = targetNodeSelect ? targetNodeSelect.value : "manager_primary";

    pBox.style.display = "block";
    pName.innerText = `正在上传: ${file.name} (${(file.size / (1024 * 1024)).toFixed(2)} MB)`;
    pBar.style.width = "0%";
    pPercent.innerText = "0%";

    const fd = new FormData();
    fd.append("file", file);
    fd.append("target_node_id", targetNodeID);

    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/archives/upload");
    if (this.token) {
      xhr.setRequestHeader("Authorization", `Bearer ${this.token}`);
    }

    xhr.upload.onprogress = e => {
      if (e.lengthComputable) {
        const percent = Math.round((e.loaded / e.total) * 100);
        pBar.style.width = percent + "%";
        pPercent.innerText = percent + "%";
      }
    };

    xhr.onload = () => {
      pBox.style.display = "none";
      if (xhr.status >= 200 && xhr.status < 300) {
        this.loadArchives();
        // 自动轮询几次以跟进解压与诊断进度
        setTimeout(() => this.loadArchives(), 2000);
        setTimeout(() => this.loadArchives(), 5000);
      } else {
        alert("上传失败: " + xhr.responseText);
      }
    };

    xhr.onerror = () => {
      pBox.style.display = "none";
      alert("上传网络错误");
    };

    xhr.send(fd);
  },

  async deleteArchive(id) {
    if (!confirm("确定要删除该日志归档包及其解压内容吗？")) return;
    await this.api(`/api/archives/${id}`, "DELETE");
    this.loadArchives();
  },

  // ================= 在线文件树与日志查看 =================

  async viewFiles(archiveID) {
    this.currentViewingArchiveID = archiveID;
    document.getElementById("viewer-filepath").innerText = "请从左侧选择文件...";
    document.getElementById("viewer-content").innerText = "";
    document.getElementById("viewer-meta").innerText = "";

    try {
      const res = await this.api(`/api/archives/${archiveID}/files`);
      if (!res.ok) {
        alert(await res.text());
        return;
      }
      const files = await res.json();
      this.renderFileTree(archiveID, files);
      this.openModal("modal-file-browser");
    } catch (e) {
      alert("获取文件树失败: " + e.message);
    }
  },

  renderFileTree(archiveID, files) {
    const container = document.getElementById("file-tree-container");
    container.innerHTML = "";

    files.forEach(f => {
      const div = document.createElement("div");
      div.className = "file-tree-item";
      div.innerHTML = `<span>${f.is_directory ? "📁" : "📄"}</span> <span>${this.escape(f.relative_path)}</span>`;

      if (!f.is_directory) {
        div.addEventListener("click", () => {
          document.querySelectorAll(".file-tree-item").forEach(el => el.classList.remove("active"));
          div.classList.add("active");
          this.loadFileContent(archiveID, f.relative_path, f.size);
        });
      }
      container.appendChild(div);
    });

    // 默认打开第一个文件
    const first = files.find(f => !f.is_directory);
    if (first) {
      this.loadFileContent(archiveID, first.relative_path, first.size);
    }
  },

  async loadFileContent(archiveID, relPath, size) {
    const viewer = document.getElementById("viewer-content");
    viewer.innerText = "正在读取日志内容...";
    document.getElementById("viewer-filepath").innerText = relPath;
    document.getElementById("viewer-meta").innerText = `大小: ${(size / 1024).toFixed(1)} KB (前 500 行)`;

    try {
      const res = await this.api(`/api/archives/${archiveID}/file-content?path=${encodeURIComponent(relPath)}&start_line=1&limit=500`);
      if (!res.ok) {
        viewer.innerText = "读取文件失败: " + (await res.text());
        return;
      }
      const data = await res.json();
      viewer.innerText = data.lines.join("\n");
    } catch (e) {
      viewer.innerText = "读取失败: " + e.message;
    }
  },

  // ================= 故障诊断报告 =================

  async viewDiagnosis(archiveID) {
    const body = document.getElementById("diag-modal-body");
    body.innerHTML = "<p style='color: var(--text-muted);'>正在加载诊断报告...</p>";
    this.openModal("modal-diagnosis");

    try {
      const res = await this.api(`/api/reports/${archiveID}`);
      if (!res.ok) {
        body.innerHTML = "<p style='color: var(--warning);'>⚠️ 诊断报告生成中或不存在，请稍候刷新。</p>";
        return;
      }
      const rep = await res.json();
      this.renderDiagnosisReport(rep);
    } catch (e) {
      body.innerHTML = `<p style='color: var(--danger);'>加载失败: ${e.message}</p>`;
    }
  },

  renderDiagnosisReport(rep) {
    const body = document.getElementById("diag-modal-body");
    const fatalCount = rep.severity_summary?.FATAL || 0;
    const critCount = rep.severity_summary?.CRITICAL || 0;
    const warnCount = rep.severity_summary?.WARNING || 0;

    let scoreColor = "var(--success)";
    if (rep.health_score < 60) scoreColor = "var(--danger)";
    else if (rep.health_score < 85) scoreColor = "var(--warning)";

    let eventsHtml = "";
    if (rep.events && rep.events.length > 0) {
      eventsHtml = rep.events.map(ev => {
        const sevClass = ev.severity.toLowerCase();
        return `
          <div class="diag-event-item ${sevClass}">
            <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px;">
              <div>
                <span class="badge badge-${sevClass}">${ev.severity}</span>
                <strong style="margin-left: 8px;">${this.escape(ev.rule_name)}</strong>
                <span style="color: var(--text-dim); font-size: 11px; margin-left: 6px;">[组件: ${ev.storage_type}]</span>
              </div>
              <span style="font-size: 11px; color: var(--text-dim);">${ev.timestamp || ''}</span>
            </div>
            <div style="font-size: 12px; color: var(--text-muted); margin-bottom: 6px;">
              位置: <code>${this.escape(ev.file_path)} : 第 ${ev.line_number} 行</code>
            </div>
            <div class="hit-line-content" style="font-size: 12px; margin-bottom: 8px;">
              ${this.escape(ev.matched_content)}
            </div>
            <div class="diag-suggestion">
              💡 <strong>专家排查处置指引:</strong> ${this.escape(ev.suggestion)}
            </div>
          </div>
        `;
      }).join("");
    } else {
      eventsHtml = `<div style="text-align: center; color: var(--success); padding: 30px;">
        <h2>🎉 未检出高危存储故障特征</h2>
        <p style="margin-top: 8px; color: var(--text-muted); font-size: 13px;">该日志包各项运行指标平稳正常！</p>
      </div>`;
    }

    body.innerHTML = `
      <div class="health-score-container">
        <div class="score-circle" style="border-color: ${scoreColor};">
          <div class="score-num" style="color: ${scoreColor};">${rep.health_score}</div>
          <div class="score-unit">系统健康分</div>
        </div>
        <div style="flex: 1;">
          <h3 style="font-size: 16px; margin-bottom: 6px;">归档日志: ${this.escape(rep.archive_name)}</h3>
          <p style="color: var(--text-muted); font-size: 13px; line-height: 1.5;">${this.escape(rep.summary_text)}</p>
          <div style="display: flex; gap: 14px; margin-top: 12px;">
            <span class="badge badge-fatal">致命致命 (FATAL): ${fatalCount}</span>
            <span class="badge badge-danger">严重错误 (CRITICAL): ${critCount}</span>
            <span class="badge badge-warning">警告异常 (WARNING): ${warnCount}</span>
          </div>
        </div>
      </div>

      <hr style="border: none; border-top: 1px solid var(--border-color); margin: 20px 0;">

      <h4 style="font-size: 14px; margin-bottom: 14px;">🔎 命中存储故障时序事件流水 (共 ${rep.total_events} 处):</h4>
      <div>${eventsHtml}</div>
    `;
  },

  // ================= 日志全局检索 =================

  populateSearchArchiveSelect() {
    const sel = document.getElementById("search-archive-select");
    sel.innerHTML = '<option value="">-- 请选择日志包 --</option>';
    this.archives.forEach(a => {
      if (a.status === "ready") {
        sel.innerHTML += `<option value="${a.id}">${this.escape(a.filename)} (${(a.size / (1024 * 1024)).toFixed(1)} MB)</option>`;
      }
    });
  },

  async doSearch(page = 1) {
    const archiveID = document.getElementById("search-archive-select").value;
    if (!archiveID) {
      alert("请先选择目标日志包");
      return;
    }

    const keyword = document.getElementById("search-keyword").value.trim();
    const level = document.getElementById("search-level").value;
    const filePath = document.getElementById("search-filepath").value.trim();
    const isRegex = document.getElementById("search-is-regex").checked;
    const caseSensitive = document.getElementById("search-case-sensitive").checked;
    const contextLines = parseInt(document.getElementById("search-context-lines").value, 10);

    const btn = document.getElementById("btn-exec-search");
    btn.disabled = true;
    btn.innerText = "检索中...";

    const resCard = document.getElementById("search-results-card");
    const hitsContainer = document.getElementById("search-hits-container");

    try {
      const res = await this.api("/api/search", "POST", {
        archive_id: archiveID,
        keyword,
        level,
        file_path: filePath,
        is_regex: isRegex,
        case_sensitive: caseSensitive,
        context_lines: contextLines,
        page,
        page_size: 50,
      });

      if (!res.ok) {
        throw new Error(await res.text());
      }

      const data = await res.json();
      resCard.style.display = "block";
      document.getElementById("search-cost-badge").innerText = `耗时 ${data.cost_ms} ms`;
      document.getElementById("search-result-title").innerText = `检索结果 (找到 ${data.total_hits} 处匹配)`;

      this.renderSearchHits(data.hits, keyword);
    } catch (e) {
      alert("检索失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.innerText = "🔎 执行检索";
    }
  },

  renderSearchHits(hits, keyword) {
    const container = document.getElementById("search-hits-container");
    container.innerHTML = "";

    if (!hits || hits.length === 0) {
      container.innerHTML = `<p style="text-align: center; color: var(--text-dim); padding: 30px;">未匹配到符合条件的日志行</p>`;
      return;
    }

    hits.forEach(h => {
      const div = document.createElement("div");
      div.className = "search-hit-item";

      let highlighted = this.escape(h.content);
      if (keyword) {
        const reg = new RegExp(`(${this.escapeRegex(keyword)})`, "gi");
        highlighted = highlighted.replace(reg, '<span class="highlight">$1</span>');
      }

      let ctxHtml = "";
      if (h.context_before && h.context_before.length > 0) {
        ctxHtml += `<div class="hit-context">${h.context_before.map(l => this.escape(l)).join("<br>")}</div>`;
      }
      ctxHtml += `<div class="hit-line-content">${highlighted}</div>`;
      if (h.context_after && h.context_after.length > 0) {
        ctxHtml += `<div class="hit-context">${h.context_after.map(l => this.escape(l)).join("<br>")}</div>`;
      }

      div.innerHTML = `
        <div class="hit-header">
          <span><code>${this.escape(h.file_path)} : 第 ${h.line_number} 行</code></span>
          <div>
            <span class="badge ${h.level === "ERROR" || h.level === "FATAL" ? "badge-danger" : h.level === "WARN" ? "badge-warning" : "badge-muted"}">${h.level}</span>
            <span style="color: var(--text-dim); font-size: 11px; margin-left: 8px;">${h.timestamp || ""}</span>
          </div>
        </div>
        ${ctxHtml}
      `;
      container.appendChild(div);
    });
  },

  // ================= 规则管理 =================

  async loadRules() {
    try {
      const res = await this.api("/api/rules");
      if (res.ok) {
        this.rules = await res.json();
        this.renderRules();
      }
    } catch (e) {
      console.error(e);
    }
  },

  renderRules() {
    const tbody = document.querySelector("#table-rules tbody");
    if (!tbody) return;
    tbody.innerHTML = "";

    this.rules.forEach(r => {
      const tr = document.createElement("tr");
      const sevClass = r.severity.toLowerCase();

      tr.innerHTML = `
        <td><strong>${this.escape(r.name)}</strong></td>
        <td><span class="badge badge-info">${r.storage_type}</span></td>
        <td><span class="badge badge-${sevClass}">${r.severity}</span></td>
        <td><code style="font-size: 11px;">${this.escape(r.pattern)}</code></td>
        <td><span class="badge badge-muted">${r.is_regex ? "正则" : "关键词"}</span></td>
        <td style="max-width: 320px; font-size: 12px; color: var(--text-muted);">${this.escape(r.suggestion)}</td>
        <td><span class="badge ${r.enabled ? "badge-success" : "badge-muted"}">${r.enabled ? "已启用" : "已禁用"}</span></td>
        <td>
          ${this.currentUser?.role === "admin" ? `
            <button class="btn btn-danger btn-sm" onclick="app.deleteRule('${r.id}')">删除</button>
          ` : '-'}
        </td>
      `;
      tbody.appendChild(tr);
    });
  },

  openCreateRuleModal() {
    document.getElementById("rule-name").value = "";
    document.getElementById("rule-pattern").value = "";
    document.getElementById("rule-desc").value = "";
    document.getElementById("rule-suggestion").value = "";
    this.openModal("modal-rule");
  },

  async submitRule() {
    const name = document.getElementById("rule-name").value.trim();
    const storageType = document.getElementById("rule-storage").value;
    const severity = document.getElementById("rule-severity").value;
    const pattern = document.getElementById("rule-pattern").value.trim();
    const isRegex = document.getElementById("rule-is-regex").checked;
    const desc = document.getElementById("rule-desc").value.trim();
    const sugg = document.getElementById("rule-suggestion").value.trim();

    try {
      const res = await this.api("/api/rules", "POST", {
        name,
        storage_type: storageType,
        severity,
        pattern,
        is_regex: isRegex,
        description: desc,
        suggestion: sugg,
        enabled: true,
      });
      if (!res.ok) throw new Error(await res.text());
      this.closeModal("modal-rule");
      this.loadRules();
    } catch (e) {
      alert("保存规则失败: " + e.message);
    }
  },

  async deleteRule(id) {
    if (!confirm("确定要删除该故障规则吗？")) return;
    await this.api(`/api/rules/${id}`, "DELETE");
    this.loadRules();
  },

  async resetDefaultRules() {
    if (!confirm("确定要恢复官方内置的 Ceph / HDFS / MinIO 预设规则库吗？")) return;
    await this.api("/api/rules/reset-defaults", "POST");
    this.loadRules();
  },

  // ================= 仪表盘统计 =================

  updateDashboardStats() {
    document.getElementById("stat-nodes").innerText = this.nodes.length || 1;
    document.getElementById("stat-archives").innerText = this.archives.length || 0;

    let totalBytes = 0;
    this.archives.forEach(a => totalBytes += a.size || 0);
    document.getElementById("stat-storage").innerText = `${(totalBytes / (1024 * 1024)).toFixed(1)} MB`;

    let eventsTotal = 0;
    this.archives.forEach(a => {
      // 估算
      if (a.status === "ready") eventsTotal += 1;
    });
    document.getElementById("stat-events").innerText = eventsTotal;
  },

  // ================= 告警系统管理 =================

  alarmFilter: "active",

  async fetchAlarmSummary() {
    try {
      const res = await this.api("/api/alarms/summary");
      if (!res.ok) return;
      const sum = await res.json();

      const topText = document.getElementById("alarm-top-text");
      const topInd = document.getElementById("alarm-top-indicator");
      const navBadge = document.getElementById("nav-alarm-badge");
      const dashActive = document.getElementById("stat-active-alarms");

      const actStat = document.getElementById("alarm-stat-active");
      const critStat = document.getElementById("alarm-stat-critical");
      const warnStat = document.getElementById("alarm-stat-warning");

      if (actStat) actStat.innerText = sum.total_active || 0;
      if (critStat) critStat.innerText = sum.critical_count || 0;
      if (warnStat) warnStat.innerText = sum.warning_count || 0;

      if (dashActive) {
        dashActive.innerText = sum.total_active || 0;
        dashActive.style.color = sum.total_active > 0 ? "#ef4444" : "var(--text-primary)";
      }

      if (sum.total_active > 0) {
        if (topText) topText.innerHTML = `<strong style="color:#ef4444;">告警: ${sum.total_active} 待处理</strong>`;
        if (topInd) {
          topInd.style.borderColor = "rgba(239, 68, 68, 0.7)";
          topInd.style.background = "rgba(239, 68, 68, 0.15)";
        }
        if (navBadge) {
          navBadge.innerText = sum.total_active;
          navBadge.style.display = "inline-block";
        }
      } else {
        if (topText) topText.innerHTML = `告警: <span style="color:#10b981;">正常 (0)</span>`;
        if (topInd) {
          topInd.style.borderColor = "var(--border-color)";
          topInd.style.background = "rgba(30,41,59,0.6)";
        }
        if (navBadge) {
          navBadge.style.display = "none";
        }
      }
    } catch (e) {
      console.warn("获取告警指标失败", e);
    }
  },

  setAlarmFilter(filter) {
    this.alarmFilter = filter;
    ["active", "all", "resolved"].forEach(f => {
      const btn = document.getElementById(`btn-alarm-filter-${f}`);
      if (btn) {
        if (f === filter) btn.classList.add("active");
        else btn.classList.remove("active");
      }
    });
    this.fetchAlarms();
  },

  async fetchAlarms() {
    const tbody = document.querySelector("#table-alarms tbody");
    if (!tbody) return;
    try {
      const res = await this.api(`/api/alarms?status=${this.alarmFilter}`);
      if (!res.ok) return;
      const list = await res.json();

      if (!list || list.length === 0) {
        tbody.innerHTML = `<tr><td colspan="8" style="text-align: center; color: var(--text-muted); padding: 24px;">🎉 当前暂无匹配的系统告警记录</td></tr>`;
        return;
      }

      tbody.innerHTML = list.map(a => {
        let sevBadge = "badge-info";
        let sevLabel = a.severity || "INFO";
        if (a.severity === "CRITICAL") {
          sevBadge = "badge-danger";
        } else if (a.severity === "WARNING") {
          sevBadge = "badge-warning";
        }

        let statusBadge = "badge-danger";
        let statusLabel = "活跃中 (未恢复)";
        if (a.status === "acknowledged") {
          statusBadge = "badge-warning";
          statusLabel = "已确认 (处理中)";
        } else if (a.status === "resolved") {
          statusBadge = "badge-success";
          statusLabel = "已恢复 / 已解除";
        }

        const countBadge = (a.count && a.count > 1) 
          ? `<span class="badge badge-warning" title="已自动去重聚合频次">${a.count}次</span>` 
          : `<span class="badge badge-info">1次</span>`;

        const firstTime = a.first_occur_at ? new Date(a.first_occur_at).toLocaleString() : "-";
        const lastTime = a.last_occur_at ? new Date(a.last_occur_at).toLocaleString() : "-";

        let actionBtns = ``;
        if (a.status !== "resolved") {
          if (a.status !== "acknowledged") {
            actionBtns += `<button class="btn btn-secondary btn-sm" onclick="app.acknowledgeAlarm('${a.id}')" title="知晓并确认该告警">确认</button> `;
          }
          actionBtns += `<button class="btn btn-primary btn-sm" onclick="app.manualResolveAlarm('${a.id}')" title="手动解除此告警">解除</button> `;
        }
        actionBtns += `<button class="btn btn-danger btn-sm" onclick="app.deleteAlarm('${a.id}')" title="删除记录">🗑️</button>`;

        return `
          <tr>
            <td><span class="badge ${sevBadge}">${sevLabel}</span></td>
            <td><code style="font-size: 12px;">${this.escape(a.alarm_type)}</code></td>
            <td>
              <strong>${this.escape(a.node_name || a.node_id)}</strong>
              <div style="font-size: 11px; color: var(--text-muted);">${this.escape(a.node_ip || '')}</div>
            </td>
            <td>
              <div style="font-weight: 600; color: var(--text-primary); margin-bottom: 2px;">${this.escape(a.title)}</div>
              <div style="font-size: 12px; color: var(--text-secondary); line-height: 1.4;">${this.escape(a.message)}</div>
            </td>
            <td>${countBadge}</td>
            <td style="font-size: 11px; color: var(--text-muted);">
              <div>初次: ${firstTime}</div>
              <div>最新: ${lastTime}</div>
            </td>
            <td><span class="badge ${statusBadge}">${statusLabel}</span></td>
            <td><div style="display: flex; gap: 4px;">${actionBtns}</div></td>
          </tr>
        `;
      }).join("");
    } catch (e) {
      console.warn("加载告警列表失败", e);
    }
  },

  async acknowledgeAlarm(id) {
    try {
      const res = await this.api(`/api/alarms/${id}/ack`, "POST");
      if (!res.ok) throw new Error(await res.text());
      await this.fetchAlarms();
      await this.fetchAlarmSummary();
    } catch (e) {
      alert("确认告警失败: " + e.message);
    }
  },

  async manualResolveAlarm(id) {
    if (!confirm("确定要手动将该告警标记为已解除恢复吗？")) return;
    try {
      const res = await this.api(`/api/alarms/${id}/resolve`, "POST");
      if (!res.ok) throw new Error(await res.text());
      await this.fetchAlarms();
      await this.fetchAlarmSummary();
    } catch (e) {
      alert("解除告警失败: " + e.message);
    }
  },

  async deleteAlarm(id) {
    if (!confirm("确定删除该告警记录吗？")) return;
    try {
      const res = await this.api(`/api/alarms/${id}`, "DELETE");
      if (!res.ok) throw new Error(await res.text());
      await this.fetchAlarms();
      await this.fetchAlarmSummary();
    } catch (e) {
      alert("删除告警失败: " + e.message);
    }
  },

  async clearResolvedAlarms() {
    if (!confirm("确定要清空所有状态为【已恢复】的历史告警记录吗？")) return;
    try {
      const res = await this.api(`/api/alarms/clear-resolved`, "POST");
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      alert("✅ " + data.message);
      await this.fetchAlarms();
      await this.fetchAlarmSummary();
    } catch (e) {
      alert("清理已恢复告警失败: " + e.message);
    }
  },

  // ================= 辅助工具 =================

  openModal(id) {
    document.getElementById(id).classList.add("active");
  },

  closeModal(id) {
    document.getElementById(id).classList.remove("active");
  },

  escape(str) {
    if (!str) return "";
    return String(str).replace(/[&<>"']/g, m => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&#39;",
    }[m]));
  },

  escapeRegex(str) {
    return str.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  },
};

window.addEventListener("DOMContentLoaded", () => app.init());
