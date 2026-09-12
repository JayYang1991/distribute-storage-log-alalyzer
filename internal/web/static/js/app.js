// 分布式存储日志分析系统 前端应用核心逻辑 (原生纯JS，零外部网络依赖)

const app = {
  token: localStorage.getItem("token") || "",
  currentUser: null,
  activeTab: "dashboard",
  archives: [],
  archiveFilterText: "",
  nodes: [],
  users: [],
  rules: [],
  currentViewingArchiveID: null,
  currentErrorArchiveID: null,

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

    // 归档标签输入实时查重与唯一性校验
    this.bindTagValidation();

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
      this.closeAllModals();
      this.updateUserUI();
      this.refreshData();
    } catch (err) {
      errBox.innerText = err.message;
      errBox.style.display = "block";
    }
  },

  fillLoginForm(username, password) {
    const uInput = document.getElementById("login-username");
    const pInput = document.getElementById("login-password");
    if (uInput) uInput.value = username;
    if (pInput) pInput.value = password;
    const errBox = document.getElementById("login-error");
    if (errBox) errBox.style.display = "none";
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
    const isAdmin = this.currentUser.role === "admin";

    document.getElementById("user-name").innerText = this.currentUser.username;
    document.getElementById("user-role").innerText = isAdmin ? "系统超级管理员" : "业务分析用户";
    document.getElementById("user-avatar").innerText = this.currentUser.username.substring(0, 1).toUpperCase();

    // 1. 侧边栏导航双视图维护：根据 data-role 控制显隐
    // 管理员：展示全部功能；普通业务用户：只展示日志上传与分析(archives, search)，不展示运维与配置菜单
    document.querySelectorAll(".nav-item").forEach(item => {
      const requiredRole = item.dataset.role;
      if (requiredRole === "admin") {
        item.style.display = isAdmin ? "flex" : "none";
      } else {
        item.style.display = "flex";
      }
    });

    // 2. 顶部运维状态指示器：普通用户视图下隐藏
    const alarmIndicator = document.getElementById("alarm-top-indicator");
    const haIndicator = document.getElementById("ha-top-indicator");
    if (alarmIndicator) alarmIndicator.style.display = isAdmin ? "flex" : "none";
    if (haIndicator) haIndicator.style.display = isAdmin ? "flex" : "none";

    // 3. 页面内配置与修改入口隔离
    const adminRuleActions = document.getElementById("admin-rule-actions");
    if (adminRuleActions) adminRuleActions.style.display = isAdmin ? "flex" : "none";

    // 4. 路由隔离与默认着陆页：
    // 普通业务用户登录时，自动切换到“日志归档与文件”视图；管理员默认停留在概览仪表盘
    const allowedUserTabs = ["archives", "search"];
    if (!isAdmin && !allowedUserTabs.includes(this.activeTab)) {
      this.switchTab("archives");
    } else if (isAdmin && (!this.activeTab || this.activeTab === "")) {
      this.switchTab("dashboard");
    }
  },

  // ================= 导航切换 =================

  switchTab(tabName) {
    // 权限守卫：非管理员试图访问管理/配置视图时，强制拦截并保留在日志分析视图
    const isAdmin = this.currentUser ? this.currentUser.role === "admin" : false;
    const allowedUserTabs = ["archives", "search", "diff"];
    if (!isAdmin && !allowedUserTabs.includes(tabName)) {
      console.warn(`[Permission Denied] 用户无权访问视图: ${tabName}，自动重定向至日志归档分析视图`);
      tabName = "archives";
    }

    this.activeTab = tabName;
    document.querySelectorAll(".nav-item").forEach(item => {
      item.classList.toggle("active", item.dataset.tab === tabName);
    });

    const titles = {
      dashboard: ["概览仪表盘", "实时监控分布式系统与各计算节点状态及日志分析全局指标"],
      nodes: ["集群节点与安装", "管理分布式计算节点与一键 SSH 远程部署业务组件"],
      users: ["用户与存储空间", "多租户权限控制与用户独立存储空间沙箱配额管理"],
      archives: ["日志归档与文件", "常用格式压缩包上传、解包文件树目录与日志在线查看"],
      search: ["日志全局检索", "毫秒级正则与全文检索引擎，时序直方图与 TraceID 链路下钻穿透"],
      rules: ["故障规则与诊断", "配置分布式系统故障特征库，自动诊断与自愈排查指导"],
      diff: ["基准差分对比", "双日志包横向对比，全方位定位文件增减、告警激增及全新异常日志模式 (Zero-Shot Patterns)"],
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
    } else if (tabName === "diff") {
      this.populateDiffArchiveSelects();
    } else if (tabName === "alarms" && isAdmin) {
      this.fetchAlarms();
    }
  },

  // ================= 数据刷新 =================

  async refreshData() {
    const isAdmin = this.currentUser && this.currentUser.role === "admin";
    if (isAdmin) {
      await Promise.all([
        this.loadNodes(),
        this.loadArchives(),
        this.loadRules(),
        this.fetchHAStatus(),
        this.fetchAlarmSummary(),
        this.loadUsers(),
      ]);
      if (this.activeTab === "alarms") {
        await this.fetchAlarms();
      }
      this.updateDashboardStats();
    } else {
      // 普通业务用户视图：仅加载归档包列表和规则特征库供分析
      await Promise.all([
        this.loadNodes(),
        this.loadArchives(),
        this.loadRules(),
      ]);
    }
  },

  async silentRefresh() {
    const isAdmin = this.currentUser && this.currentUser.role === "admin";
    if (isAdmin) {
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
    } else {
      await this.loadArchives(true);
    }
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

  async api(url, method = "GET", body = null, options = {}) {
    const headers = options.headers || {};
    if (this.token) {
      headers["Authorization"] = `Bearer ${this.token}`;
    }
    if (body && !(body instanceof FormData)) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    }
    return fetch(url, { method, headers, body, ...options });
  },

  // ================= 节点管理 =================

  async loadNodes(silent = false) {
    try {
      const res = await this.api("/api/nodes");
      if (res.ok) {
        const data = await res.json();
        this.nodes = Array.isArray(data) ? data : [];
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

    // 筛选所有在线计算节点
    const workers = this.nodes.filter(n => n.role === "worker" && n.status === "online");

    // 核心算法：按已使用容量升序排序 (容量最低的优先排在前列)
    workers.sort((a, b) => {
      const usedA = (a.resource && a.resource.disk_used_mb) ? a.resource.disk_used_mb : (a.storage_used_bytes ? Math.round(a.storage_used_bytes / (1024 * 1024)) : 0);
      const usedB = (b.resource && b.resource.disk_used_mb) ? b.resource.disk_used_mb : (b.storage_used_bytes ? Math.round(b.storage_used_bytes / (1024 * 1024)) : 0);
      if (usedA !== usedB) return usedA - usedB;
      const freeA = a.resource?.disk_free_mb || 0;
      const freeB = b.resource?.disk_free_mb || 0;
      return freeB - freeA; // 剩余可用空间大的优先
    });

    let opts = "";
    if (workers.length > 0) {
      const lowest = workers[0];
      const lowestUsed = lowest.resource?.disk_used_mb ? `${(lowest.resource.disk_used_mb / 1024).toFixed(2)} GB` : '0 MB';
      opts += `<option value="auto">🎯 智能容量调度 (推荐: 优先存放至已用容量最低节点: ${this.escape(lowest.name)}，已用 ${lowestUsed})</option>`;

      workers.forEach((n, idx) => {
        const usedMB = (n.resource && n.resource.disk_used_mb) ? n.resource.disk_used_mb : (n.storage_used_bytes ? Math.round(n.storage_used_bytes / (1024 * 1024)) : 0);
        const usedStr = usedMB >= 1024 ? `${(usedMB / 1024).toFixed(2)} GB` : `${usedMB} MB`;
        const freeGB = n.resource?.disk_free_mb ? `${(n.resource.disk_free_mb / 1024).toFixed(1)} GB` : '未知';
        const pctStr = n.resource?.disk_used_percent ? ` (${n.resource.disk_used_percent.toFixed(1)}%)` : '';
        const recBadge = (idx === 0) ? ' ⭐ [推荐: 已用容量最低]' : '';
        const diskInfo = n.disk_device ? ` [磁盘: ${n.disk_device}]` : '';

        opts += `<option value="${n.id}">${this.escape(n.name)} - ${n.ip}:${n.port}${diskInfo} (已用: ${usedStr}${pctStr}, 剩余可用: ${freeGB})${recBadge}</option>`;
      });
    } else {
      opts = '<option value="" disabled selected>⚠️ 当前无可用业务存储节点 (日志只能存入业务存储，禁止存入管理系统盘)</option>';
    }

    sel.innerHTML = opts;

    if (currentVal && Array.from(sel.options).some(o => o.value === currentVal && !o.disabled)) {
      sel.value = currentVal;
    } else if (workers.length > 0) {
      sel.value = "auto";
    }
  },

  renderNodes() {
    const tbody = document.querySelector("#table-nodes tbody");
    if (!tbody) return;
    tbody.innerHTML = "";

    this.nodes.forEach(n => {
      const tr = document.createElement("tr");
      const isOnline = n.status === "online";
      const isMaintenance = n.status === "maintenance";
      const statusBadge = isOnline
        ? '<span class="badge badge-success">在线</span>'
        : isMaintenance
        ? '<span class="badge badge-warning" title="维护模式 (Drain)：暂停新日志分配，存量查询正常">🛠️ 维护中</span>'
        : n.status === "installing"
        ? '<span class="badge badge-warning">正在安装</span>'
        : '<span class="badge badge-danger">离线</span>';

      const memStr = n.resource ? `${n.resource.mem_used_mb || 0} / ${n.resource.mem_total_mb || 0} MB` : "-";
      
      // 丰富容量展示：已用容量与剩余空间
      const usedMB = (n.resource && n.resource.disk_used_mb) ? n.resource.disk_used_mb : (n.storage_used_bytes ? Math.round(n.storage_used_bytes / (1024 * 1024)) : 0);
      const usedStr = usedMB >= 1024 ? `${(usedMB / 1024).toFixed(2)} GB` : `${usedMB} MB`;
      const freeStr = n.resource?.disk_free_mb ? `${(n.resource.disk_free_mb / 1024).toFixed(1)} GB` : "-";
      const pctStr = n.resource?.disk_used_percent ? `${n.resource.disk_used_percent.toFixed(1)}%` : "";
      
      const diskHtml = (n.resource && n.resource.disk_total_mb > 0)
        ? `<div><strong>${freeStr} 可用</strong></div><div style="font-size: 11px; color: var(--text-muted);">已用 ${usedStr} (${pctStr})</div>`
        : `<div>${freeStr} 可用</div><div style="font-size: 11px; color: var(--text-muted);">日志占用: ${usedStr}</div>`;

      const lastHb = n.last_heartbeat ? new Date(n.last_heartbeat).toLocaleTimeString() : "-";
      const diskBadge = n.disk_device ? `<span style="font-size: 11px; color: var(--primary); display: block;">💾 ${this.escape(n.disk_device)} (${n.fs_type || 'ext4'})</span>` : '';

      tr.innerHTML = `
        <td><strong>${this.escape(n.name)}</strong>${diskBadge}</td>
        <td><span class="badge ${n.role === "manager" ? "badge-info" : "badge-muted"}">${n.role}</span></td>
        <td><code>${this.escape(n.ip)}:${n.port}</code></td>
        <td>${statusBadge}</td>
        <td>${n.active_tasks || 0}</td>
        <td>${memStr}</td>
        <td>${diskHtml}</td>
        <td>${lastHb}</td>
        <td>
          ${n.role !== "manager" && this.currentUser?.role === "admin" ? `
            <button class="btn btn-sm ${isMaintenance ? 'btn-success' : 'btn-warning'}" onclick="app.toggleNodeMaintenance('${n.id}', ${!isMaintenance})" title="${isMaintenance ? '点击恢复正常上线' : '点击进入维护模式 (暂停新日志调度分配)'}">${isMaintenance ? '✅ 上线' : '🛠️ 维护'}</button>
            <button class="btn btn-danger btn-sm" onclick="app.openRemoveNodeModal('${n.id}')">移除</button>
          ` : '<span style="color: var(--text-dim); font-size: 11px;">核心管理节点</span>'}
        </td>
      `;
      tbody.appendChild(tr);
    });
  },

  async toggleNodeMaintenance(nodeID, maintenance) {
    const actionName = maintenance ? "进入维护模式 (暂停分配新日志包)" : "恢复正常上线";
    if (!confirm(`确定要将该节点设为【${actionName}】吗？`)) return;
    try {
      const res = await this.api("/api/nodes/maintenance", "POST", { node_id: nodeID, maintenance });
      if (!res.ok) {
        alert("操作失败: " + (await res.text()));
        return;
      }
      this.loadNodes();
    } catch (e) {
      alert("网络异常: " + e.message);
    }
  },

  async downloadDatabaseBackup() {
    if (!confirm("确定要导出当前元数据库 (bbolt) 实时一致性热快照吗？")) return;
    try {
      const token = localStorage.getItem("token") || "";
      const res = await fetch(`/api/system/backup?token=${encodeURIComponent(token)}`, {
        headers: {
          "Authorization": `Bearer ${token}`
        }
      });
      if (!res.ok) {
        const text = await res.text();
        alert("导出元数据快照失败: " + text);
        return;
      }
      const blob = await res.blob();
      const disposition = res.headers.get("Content-Disposition") || "";
      let filename = `analyzer-backup-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-")}.db`;
      const match = disposition.match(/filename="?([^";]+)"?/);
      if (match && match[1]) {
        filename = match[1];
      }
      const url = window.URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.style.display = "none";
      a.href = url;
      a.download = filename;
      document.body.appendChild(a);
      a.click();
      window.URL.revokeObjectURL(url);
      document.body.removeChild(a);
    } catch (e) {
      alert("下载快照发生异常: " + e.message);
    }
  },

  openDeployModal() {
    document.getElementById("deploy-terminal-box").style.display = "none";
    document.getElementById("btn-deploy-submit").disabled = false;
    const workerPortInput = document.getElementById("deploy-worker-port");
    if (workerPortInput) workerPortInput.value = 8081;
    document.getElementById("deploy-disk-input").value = "";
    document.getElementById("deploy-disk-select").innerHTML = '<option value="">-- 请点击“探测目标磁盘”或手动在下方输入设备路径 --</option>';
    const listBox = document.getElementById("deploy-disk-list-box");
    if (listBox) {
      listBox.innerHTML = "";
      listBox.style.display = "none";
    }
    const hint = document.getElementById("deploy-disk-hint");
    if (hint) {
      hint.style.display = "none";
    }
    this._lastDetectedDisks = [];
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
      const listBox = document.getElementById("deploy-disk-list-box");

      select.innerHTML = '<option value="">-- 请选择检测到的物理硬盘 (亦可上方多选) --</option>';

      if (!disks || disks.length === 0) {
        select.innerHTML = '<option value="">未检测到物理磁盘，请检查目标主机权限</option>';
        if (listBox) listBox.style.display = "none";
        if (hint) {
          hint.style.display = "block";
          hint.style.background = "rgba(245, 158, 11, 0.15)";
          hint.style.border = "1px solid #f59e0b";
          hint.style.color = "#fbbf24";
          hint.innerText = "⚠️ 未检测到可用的独立物理磁盘设备，系统严禁使用系统盘，请确认目标主机已挂载物理数据盘。";
        }
      } else {
        let listHtml = '';
        let checkedDisks = [];

        disks.forEach((d, idx) => {
          const modelInfo = d.model ? ` [${d.model}]` : '';
          const isSys = d.is_system;
          const isSafe = d.can_format && !isSys;
          
          if (isSafe && !isSys) {
            checkedDisks.push(d.path);
          }

          listHtml += `
            <label style="display: flex; align-items: center; justify-content: space-between; padding: 6px 8px; margin-bottom: 4px; border-radius: 4px; background: ${isSys ? 'rgba(239, 68, 68, 0.1)' : 'rgba(255, 255, 255, 0.04)'}; cursor: ${isSys ? 'not-allowed' : 'pointer'};">
              <span style="display: flex; align-items: center; gap: 8px;">
                <input type="checkbox" name="deploy_disk_cb" value="${d.path}" ${isSys ? 'disabled' : (isSafe ? 'checked' : '')} onchange="app.onDiskCheckboxChange()">
                <strong style="color: ${isSys ? '#f87171' : (isSafe ? '#34d399' : '#e2e8f0')}">${d.path}</strong>
                <span style="font-size: 11px; color: var(--text-muted);">(${d.size})${modelInfo}</span>
              </span>
              <span style="font-size: 11px; color: ${isSys ? '#f87171' : (isSafe ? '#34d399' : '#fbbf24')};">
                ${isSys ? '🚫 严禁使用 (系统盘)' : (isSafe ? '✅ 安全裸盘 (推荐)' : '⚠️ 已有分区/FS')}
              </span>
            </label>
          `;

          if (isSafe && !isSys) {
            select.innerHTML += `<option value="${d.path}">✅ [安全裸盘 (推荐)] ${d.path} (${d.size})${modelInfo}</option>`;
          } else {
            select.innerHTML += `<option value="${d.path}" ${isSys ? 'disabled' : ''} style="color: #94a3b8;">${isSys ? '🚫 [系统关键盘]' : '⚠️ [已有数据/分区]'} ${d.path} (${d.size})${modelInfo} - ${d.status_text || ''}</option>`;
          }
        });

        if (listBox) {
          listBox.innerHTML = listHtml;
          listBox.style.display = "block";
        }

        diskInput.value = checkedDisks.join(", ");

        if (hint) {
          hint.style.display = "block";
          if (checkedDisks.length > 0) {
            hint.style.background = "rgba(16, 185, 129, 0.15)";
            hint.style.border = "1px solid #10b981";
            hint.style.color = "#34d399";
            hint.innerHTML = `<strong>✅ 智能多盘防呆已就绪：</strong>已优选 ${checkedDisks.length} 块纯净裸物理盘 (<code>${checkedDisks.join(", ")}</code>)。系统盘已被自动标红锁定，系统将为勾选的各盘分别启动独立 Worker 进程。`;
          } else {
            hint.style.background = "rgba(239, 68, 68, 0.15)";
            hint.style.border = "1px solid #ef4444";
            hint.style.color = "#f87171";
            hint.innerHTML = `<strong>⚠️ 保护提示：</strong>未检测到纯净裸盘，系统关键分区已被强制锁定！请勾选独立数据盘，或手动提供专用存储盘设备路径。`;
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

  onDiskCheckboxChange() {
    const cbs = document.querySelectorAll('input[name="deploy_disk_cb"]:checked');
    const vals = Array.from(cbs).map(c => c.value);
    document.getElementById("deploy-disk-input").value = vals.join(", ");
  },

  onDiskSelectChange() {
    const val = document.getElementById("deploy-disk-select").value;
    if (val) {
      if (this._lastDetectedDisks) {
        const target = this._lastDetectedDisks.find(d => d.path === val || d.name === val);
        if (target && target.is_system) {
          alert(`【安全防呆拦截】${val} 属于系统关键分区，严禁选用！`);
          document.getElementById("deploy-disk-select").value = "";
          return;
        }
      }
      const cur = document.getElementById("deploy-disk-input").value.trim();
      const set = new Set(cur ? cur.split(",").map(s => s.trim()).filter(Boolean) : []);
      set.add(val);
      document.getElementById("deploy-disk-input").value = Array.from(set).join(", ");
    }
  },

  async submitDeploy() {
    const host = document.getElementById("deploy-host").value.trim();
    const port = parseInt(document.getElementById("deploy-port").value, 10);
    const workerPortInput = document.getElementById("deploy-worker-port");
    const workerPort = parseInt(workerPortInput ? workerPortInput.value : "8081", 10) || 8081;
    const username = document.getElementById("deploy-user").value.trim();
    const password = document.getElementById("deploy-pass").value;
    const nodeName = document.getElementById("deploy-name").value.trim();

    const diskInputVal = document.getElementById("deploy-disk-input").value.trim();
    const selectedDisks = diskInputVal ? diskInputVal.split(',').map(s => s.trim()).filter(Boolean) : [];

    // 强制校验：严禁使用系统盘 & 必须选盘
    if (selectedDisks.length === 0) {
      alert("【安全架构限制】严禁使用系统盘存放日志！\n增加 Worker 节点时必须至少指定一块独立的物理存储盘。\n请点击“探测目标磁盘”并勾选物理裸盘，或手动输入设备路径（如 /dev/sdb）。");
      return;
    }

    // 核心唯一性防呆校验：以 IP 和端口作为唯一标识，禁止重复添加！
    if (this.nodes && this.nodes.length > 0) {
      const diskCount = Math.max(1, selectedDisks.length);
      for (let i = 0; i < diskCount; i++) {
        const checkPort = workerPort + i;
        const exists = this.nodes.find(n => n.role === "worker" && n.ip === host && n.port === checkPort && n.status !== "failed");
        if (exists) {
          alert(`【禁止重复添加】业务组件节点 [${host}:${checkPort}] 已存在于集群中 (名称: ${exists.name}, 状态: ${exists.status})，禁止重复添加！`);
          return;
        }
      }
    }

    const fsType = document.getElementById("deploy-fstype").value;
    const mountPoint = document.getElementById("deploy-mountpoint").value.trim();
    const formatDisk = document.getElementById("deploy-format-disk").checked;

    // 前端防呆校验：系统盘拦截
    if (this._lastDetectedDisks && this._lastDetectedDisks.length > 0) {
      for (const dPath of selectedDisks) {
        const matchDisk = this._lastDetectedDisks.find(d => d.path === dPath || d.name === dPath);
        if (matchDisk && matchDisk.is_system) {
          alert(`【安全防呆拦截】目标磁盘 ${dPath} 属于系统关键分区，严禁用于日志存储！已中止提交以防系统崩溃。`);
          return;
        }
      }
    }

    const termBox = document.getElementById("deploy-terminal-box");
    const term = document.getElementById("deploy-terminal");
    const btn = document.getElementById("btn-deploy-submit");

    termBox.style.display = "block";
    const endPort = workerPort + selectedDisks.length - 1;
    const portRangeStr = selectedDisks.length > 1 ? `${workerPort}~${endPort}` : `${workerPort}`;
    term.innerText = `[1/4] 准备向目标 ${host}:${port} 发起 SSH 远程一键部署 (业务服务端口: ${portRangeStr})...\n` +
      `[多盘配置] 共选定 ${selectedDisks.length} 块物理硬盘: ${selectedDisks.join(', ')}\n` +
      `[架构机制] 将为各盘分别拉起独立 Worker 进程实例 (服务端口: ${portRangeStr}) 实现 I/O 隔离\n` +
      `[2/4] 正在传输安装包并执行磁盘格式化与挂载，请稍候...\n`;
    btn.disabled = true;

    try {
      const res = await this.api("/api/nodes/deploy", "POST", {
        host,
        port,
        worker_port: workerPort,
        username,
        password,
        node_name: nodeName,
        disk_device: selectedDisks.join(","),
        disk_devices: selectedDisks,
        fs_type: fsType,
        mount_point: mountPoint,
        format_disk: formatDisk,
      });
      if (!res.ok) {
        const err = await res.text();
        throw new Error(err);
      }
      term.innerText += `[3/4] 远程磁盘配置与多 Worker 实例启动指令执行成功，等待各实例注册上线...\n`;
      
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

  openRemoveNodeModal(id) {
    const nodes = Array.isArray(this.nodes) ? this.nodes : [];
    const node = nodes.find(n => n && n.id === id);
    if (!node) return;

    document.getElementById("remove-node-id").value = id;
    document.getElementById("remove-node-info").innerText = `节点名称: ${node.name || id} (IP: ${node.ip || '127.0.0.1'}, 端口: ${node.port})`;

    // 计算当前存储日志包数量和空间
    const archives = Array.isArray(this.archives) ? this.archives : [];
    const nodeArchives = archives.filter(a => a && a.storage_node_id === id);
    const totalBytes = nodeArchives.reduce((acc, a) => acc + (a.size || 0), 0);
    const sizeStr = (totalBytes / 1024 / 1024).toFixed(1) + " MB";
    document.getElementById("remove-node-stats").innerText = `当前物理存储日志包: ${nodeArchives.length} 个 | 占用存储: ${sizeStr}`;

    // 重置策略为 migrate
    const radios = document.getElementsByName("remove-node-action");
    radios.forEach(r => r.checked = (r.value === "migrate"));
    document.getElementById("group-target-node").style.display = "block";
    document.getElementById("remove-node-progress").style.display = "none";
    document.getElementById("btn-confirm-remove-node").disabled = false;

    // 填充目标候选节点列表：过滤掉自身
    const targetSelect = document.getElementById("remove-target-node");
    targetSelect.innerHTML = "";

    // 候选的 worker 节点 (排序：已用容量升序，优先推荐容量最低的存活节点)
    const candidateWorkers = nodes
      .filter(n => n && n.id !== id && n.role !== "manager" && n.status === "online")
      .sort((a, b) => (a.resource?.disk_used_mb || 0) - (b.resource?.disk_used_mb || 0));

    if (candidateWorkers.length > 0) {
      candidateWorkers.forEach((n, idx) => {
        const opt = document.createElement("option");
        opt.value = n.id;
        const usedMB = n.resource?.disk_used_mb || 0;
        const freeMB = n.resource?.disk_free_mb || 0;
        opt.text = `${idx === 0 ? '⭐ [推荐最低容量] ' : ''}${n.name || n.id} (${n.ip}:${n.port} - 已用: ${usedMB} MB, 剩余: ${freeMB} MB)`;
        targetSelect.appendChild(opt);
      });
    } else {
      const optEmpty = document.createElement("option");
      optEmpty.value = "";
      optEmpty.disabled = true;
      optEmpty.selected = true;
      optEmpty.text = "⚠️ 无其他在线业务存储节点可供迁移 (禁止迁移至管理节点系统盘)";
      targetSelect.appendChild(optEmpty);
    }

    this.openModal("modal-remove-node");
  },

  onRemoveActionChange() {
    const radios = document.getElementsByName("remove-node-action");
    let selectedAction = "migrate";
    for (const r of radios) {
      if (r.checked) {
        selectedAction = r.value;
        break;
      }
    }
    const groupTarget = document.getElementById("group-target-node");
    const targetSelect = document.getElementById("remove-target-node");
    if (selectedAction === "migrate") {
      groupTarget.style.display = "block";
      targetSelect.required = true;
    } else {
      groupTarget.style.display = "none";
      targetSelect.required = false;
    }
  },

  async confirmRemoveNode() {
    const id = document.getElementById("remove-node-id").value;
    if (!id) return;

    const radios = document.getElementsByName("remove-node-action");
    let action = "migrate";
    for (const r of radios) {
      if (r.checked) {
        action = r.value;
        break;
      }
    }

    const targetNodeSelect = document.getElementById("remove-target-node");
    const targetNodeID = targetNodeSelect ? targetNodeSelect.value : "";

    if (action === "migrate" && (!targetNodeID || targetNodeID === "manager_primary" || targetNodeID === "local")) {
      alert("⚠️ 当前无其他可用在线业务存储节点！日志只能保存在业务存储节点上，禁止迁移至管理节点系统盘。请先接入新业务节点或选择其他移除方式。");
      return;
    }

    const btn = document.getElementById("btn-confirm-remove-node");
    const progressBox = document.getElementById("remove-node-progress");
    btn.disabled = true;
    progressBox.style.display = "block";
    progressBox.innerText = action === "migrate" ? "⏳ 正在平滑迁移日志文件至目标业务节点并更新索引，请稍候..." : "⏳ 正在执行节点移除处置，请稍候...";

    try {
      const res = await this.api(`/api/nodes/${id}/remove`, "POST", {
        action: action,
        target_node_id: targetNodeID,
      });

      if (!res.ok) {
        const errText = await res.text();
        throw new Error(errText || "服务端返回异常");
      }

      const data = await res.json().catch(() => ({}));
      alert(`🎉 ${data.message || "节点已成功移除！"}`);

      this.closeModal("modal-remove-node");
      await this.refreshData();
    } catch (err) {
      alert(`❌ 移除节点失败: ${err.message}`);
    } finally {
      btn.disabled = false;
      progressBox.style.display = "none";
    }
  },

  async deleteNode(id) {
    this.openRemoveNodeModal(id);
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
        const data = await res.json();
        this.archives = Array.isArray(data) ? data : [];
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

      let list = isRecentOnly ? this.archives.slice(0, 5) : this.archives;
      if (!isRecentOnly && this.archiveFilterText) {
        const q = this.archiveFilterText.toLowerCase();
        list = list.filter(a => {
          if (a.filename && a.filename.toLowerCase().includes(q)) return true;
          if (a.remark && a.remark.toLowerCase().includes(q)) return true;
          if (a.tags && a.tags.some(t => t.toLowerCase().includes(q))) return true;
          return false;
        });
      }

      if (list.length === 0) {
        if (!isRecentOnly && this.archiveFilterText) {
          tbody.innerHTML = `<tr><td colspan="10" style="text-align: center; color: var(--text-dim); padding: 24px;">未找到匹配 “<strong>${this.escape(this.archiveFilterText)}</strong>” 的日志包，<a href="javascript:void(0)" onclick="app.clearArchiveFilter()" style="color: var(--primary); text-decoration: underline;">点击清空筛选条件</a></td></tr>`;
        } else {
          tbody.innerHTML = `<tr><td colspan="${isRecentOnly ? 7 : 10}" style="text-align: center; color: var(--text-dim); padding: 24px;">暂无日志归档包，请点击上方“上传日志包”按钮开始分析</td></tr>`;
        }
        return;
      }

      list.forEach(a => {
        const tr = document.createElement("tr");
        const sizeMB = (a.size / (1024 * 1024)).toFixed(2);
        const isReady = a.status === "ready";

        let statusBadge = "";
        if (isReady) {
          statusBadge = '<span class="badge badge-success">分析就绪</span>';
        } else if (a.status === "failed") {
          const errInfo = this.formatArchiveError(a.error_msg);
          statusBadge = `
            <div style="display: flex; flex-direction: column; gap: 4px; align-items: flex-start;">
              <span class="badge badge-danger">失败</span>
              <span class="archive-error-tag" onclick="app.showArchiveError('${a.id}')" title="点击查看详细失败原因与处置方案">
                ${errInfo.icon} ${this.escape(errInfo.title)}
              </span>
            </div>
          `;
        } else {
          statusBadge = '<span class="badge badge-warning">解包诊断中</span>';
        }

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
                <button class="btn btn-secondary btn-sm" title="下载原日志压缩包" onclick="app.downloadArchive('${a.id}')">📥 下载</button>
              ` : (a.status === 'failed' ? `
                <button class="btn btn-danger btn-sm" title="查看详细失败原因与解决方案" onclick="app.showArchiveError('${a.id}')">❓ 原因</button>
                <button class="btn btn-warning btn-sm" title="重新尝试解包与分析" onclick="app.retryArchive('${a.id}')">🔄 重试</button>
              ` : '-')}
            </td>
          `;
        } else {
          let tagsHtml = "";
          if (a.tags && a.tags.length > 0) {
            tagsHtml = `<div style="display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 2px;">` +
              a.tags.map(t => `<span class="badge-tag" onclick="app.setArchiveTagFilter('${this.escape(t)}')" title="点击筛选此标签">🏷️ ${this.escape(t)}</span>`).join("") +
              `</div>`;
          } else {
            tagsHtml = `<div style="color: var(--text-dim); font-size: 11px; margin-bottom: 2px;">无标签</div>`;
          }
          let remarkHtml = "";
          if (a.remark) {
            remarkHtml = `<div class="archive-remark" title="${this.escape(a.remark)}">📝 ${this.escape(a.remark)}</div>`;
          }

          const pinBadge = a.pinned
            ? `<span class="badge badge-warning" style="cursor: pointer;" onclick="app.toggleArchivePin('${a.id}', false)" title="已锁定保护：免除生命周期超期和磁盘高水位自愈清理。点击可解除锁定">🔒 已保护</span>`
            : `<span class="badge badge-muted" style="cursor: pointer;" onclick="app.toggleArchivePin('${a.id}', true)" title="未锁定：受保留天数与紧急水位自愈清理管理。点击可开启保护锁定">未保护</span>`;

          tr.innerHTML = `
            <td>
              <div style="display: flex; align-items: center; gap: 6px;">
                ${pinBadge}
                <strong>${this.escape(a.filename)}</strong>
              </div>
            </td>
            <td>${tagsHtml}${remarkHtml}</td>
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
                <button class="btn btn-secondary btn-sm" title="下载原日志压缩包" onclick="app.downloadArchive('${a.id}')">📥 下载</button>
              ` : ''}
              ${a.status === 'failed' ? `
                <button class="btn btn-danger btn-sm" title="查看详细失败原因与解决方案" onclick="app.showArchiveError('${a.id}')">❓ 原因</button>
                <button class="btn btn-warning btn-sm" title="重新尝试解包与分析" onclick="app.retryArchive('${a.id}')">🔄 重试</button>
              ` : ''}
              <button class="btn btn-sm ${a.pinned ? 'btn-warning' : 'btn-secondary'}" title="${a.pinned ? '点击解除保护锁定' : '点击开启保护锁定 (免除自动清理)'}" onclick="app.toggleArchivePin('${a.id}', ${!a.pinned})">${a.pinned ? '🔓 解锁' : '🔒 保护'}</button>
              <button class="btn btn-secondary btn-sm" title="修改标签与备注" onclick="app.openEditArchiveMetaModal('${a.id}')">🏷️ 标记</button>
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

  formatArchiveError(errorMsg) {
    if (!errorMsg) {
      return {
        title: "分析失败",
        icon: "⚠️",
        detail: "系统未捕获到具体的异常描述信息。",
        solution: "建议直接点击“🔄 重新调度分析”，若仍失败请检查后台系统运行日志。"
      };
    }
    const lower = errorMsg.toLowerCase();
    if (lower.includes("no space left on device") || lower.includes("space left") || lower.includes("disk full") || lower.includes("存储空间不足") || lower.includes("磁盘空间不足")) {
      return {
        title: "磁盘存储空间不足",
        icon: "💾",
        detail: "目标节点存储磁盘或操作系统根分区已无剩余可用写入空间，无法完成大文件解压缩落盘。",
        solution: "建议处置方案：\n1. 清理目标节点磁盘无用文件或扩容 LVM 存储逻辑卷（如 lvextend）；\n2. 将日志归档存储路径配置或软链接至大容量物理数据盘（如专属挂载的 SSD/HDD 盘）；\n3. 完成磁盘空间扩充后，直接点击下方“🔄 重新调度分析”一键恢复分析！"
      };
    }
    if (lower.includes("permission denied") || lower.includes("access denied") || lower.includes("权限不足")) {
      return {
        title: "磁盘目录写入权限不足",
        icon: "🔒",
        detail: "分析进程缺少向目标数据存储目录写入或创建解压文件的系统读写权限。",
        solution: "建议处置方案：请检查目标节点运行用户的读写权限（如 chown/chmod 数据存储目录）后点击重试。"
      };
    }
    if (lower.includes("connection refused") || lower.includes("timeout") || lower.includes("no route to host") || lower.includes("network") || lower.includes("节点通信")) {
      return {
        title: "存储业务节点通信异常/离线",
        icon: "🌐",
        detail: "管理节点向分派的 Worker 存储节点发起通信调度时连接超时或被拒绝连接。",
        solution: "建议处置方案：请检查对应 Worker 节点服务运行状态与端口监听（8081/8082等），确认网络通畅后点击重试。"
      };
    }
    if (lower.includes("unexpected eof") || lower.includes("invalid header") || lower.includes("corrupted") || lower.includes("损坏") || lower.includes("unrecognized archive")) {
      return {
        title: "压缩文件损坏或格式不完整",
        icon: "📦",
        detail: "日志压缩包在上传传输过程中损坏、截断或解压算法无法识别文件头结构。",
        solution: "建议处置方案：请使用 tar/gzip 等工具检查原压缩包完整性，重新打包后重新上传。"
      };
    }
    if (lower.includes("killed") || lower.includes("out of memory") || lower.includes("oom")) {
      return {
        title: "系统内存耗尽 (OOM Killed)",
        icon: "⚡",
        detail: "解压或分析过程占用了过多系统内存被操作系统内核 OOM 保护机制强制终止。",
        solution: "建议处置方案：提升虚拟机/物理机内存容量，或配置系统 Swap 交换空间后重试。"
      };
    }
    return {
      title: "解包分析失败",
      icon: "⚠️",
      detail: errorMsg.length > 80 ? errorMsg.substring(0, 80) + "..." : errorMsg,
      solution: "建议处置方案：确认存储与系统运行环境正常后点击“🔄 重新调度分析”，或核对下方底层系统错误输出排查。"
    };
  },

  showArchiveError(archiveID) {
    const a = this.archives.find(item => item.id === archiveID);
    if (!a) return;
    const errInfo = this.formatArchiveError(a.error_msg);
    document.getElementById("archive-error-icon").innerText = errInfo.icon;
    document.getElementById("archive-error-title").innerText = errInfo.title;
    document.getElementById("archive-error-detail").innerText = errInfo.detail;
    document.getElementById("archive-error-solution").innerText = errInfo.solution;
    document.getElementById("archive-error-meta").innerText = `归档包: ${a.filename} | 存储节点: ${a.storage_node_name || a.assigned_worker || '本地存储'} | 上传时间: ${new Date(a.upload_time).toLocaleString()}`;
    document.getElementById("archive-error-raw").innerText = a.error_msg || "无底层详细报错信息";
    this.currentErrorArchiveID = a.id;
    this.openModal("modal-archive-error");
  },

  retryArchiveFromModal() {
    if (this.currentErrorArchiveID) {
      this.retryArchive(this.currentErrorArchiveID);
      this.closeModal("modal-archive-error");
    }
  },

  async retryArchive(id) {
    const res = await this.api(`/api/archives/${id}/retry`, "POST");
    if (res && res.error) {
      alert("重新调度分析失败: " + res.error);
      return;
    }
    this.loadArchives();
    setTimeout(() => this.loadArchives(), 2000);
    setTimeout(() => this.loadArchives(), 5000);
  },

  filterArchives(val) {
    this.archiveFilterText = (val || "").trim();
    const btnClear = document.getElementById("btn-clear-archive-filter");
    if (btnClear) {
      btnClear.style.display = this.archiveFilterText ? "inline-block" : "none";
    }
    this.renderArchives();
  },

  clearArchiveFilter() {
    this.archiveFilterText = "";
    const input = document.getElementById("archive-filter-input");
    if (input) input.value = "";
    const btnClear = document.getElementById("btn-clear-archive-filter");
    if (btnClear) btnClear.style.display = "none";
    this.renderArchives();
  },

  setArchiveTagFilter(tag) {
    const input = document.getElementById("archive-filter-input");
    if (input) input.value = tag;
    this.filterArchives(tag);
  },

  openEditArchiveMetaModal(id) {
    const a = this.archives.find(item => item.id === id);
    if (!a) return;
    document.getElementById("archive-meta-id").value = a.id;
    document.getElementById("archive-meta-filename").value = a.filename;
    const tagsInput = document.getElementById("archive-meta-tags");
    tagsInput.value = (a.tags || []).join(", ");
    tagsInput.classList.remove("input-error", "input-valid");
    const tip = document.getElementById("archive-meta-tag-tip");
    if (tip) {
      tip.style.display = "none";
      tip.innerText = "";
    }
    document.getElementById("archive-meta-remark").value = a.remark || "";
    this.openModal("modal-archive-meta");
  },

  async submitArchiveMeta() {
    const id = document.getElementById("archive-meta-id").value;
    const tagsInput = document.getElementById("archive-meta-tags");
    const tagsVal = tagsInput ? tagsInput.value.trim() : "";
    const remarkVal = document.getElementById("archive-meta-remark").value;

    if (!tagsVal) {
      alert("⚠️ 归档标签不能为空，标签为归档的唯一标识！");
      tagsInput?.focus();
      return;
    }

    try {
      const checkRes = await this.api(`/api/archives/check-tag?tag=${encodeURIComponent(tagsVal)}&exclude_id=${encodeURIComponent(id)}`);
      if (checkRes.ok) {
        const checkData = await checkRes.json();
        if (checkData.exists) {
          alert(`⚠️ 标签 “${checkData.tag || tagsVal}” 已被归档包 [${checkData.matched_filename || '其他归档'}] 占用，归档标签必须保持全系统唯一！`);
          tagsInput?.classList.add("input-error");
          tagsInput?.focus();
          return;
        }
      }
    } catch (e) {}

    const res = await this.api(`/api/archives/${id}`, "PUT", {
      tags: tagsVal,
      remark: remarkVal
    });

    if (!res.ok) {
      alert("更新标签备注失败: " + (await res.text()));
      return;
    }

    this.closeModal("modal-archive-meta");
    this.loadArchives();
  },

  // 归档标签实时查重与唯一性校验交互绑定
  bindTagValidation() {
    const uploadTagsInput = document.getElementById("upload-tags");
    const uploadTip = document.getElementById("upload-tag-tip");
    let uploadTimer = null;

    if (uploadTagsInput && uploadTip) {
      const checkUploadTag = async () => {
        const val = uploadTagsInput.value.trim();
        if (!val) {
          uploadTagsInput.classList.remove("input-error", "input-valid");
          uploadTip.style.display = "none";
          uploadTip.className = "";
          uploadTip.innerText = "";
          return;
        }

        // 优先本地快速比对现有归档
        const localDup = (this.archives || []).find(a => 
          (a.tags || []).some(t => t.trim().toLowerCase() === val.toLowerCase())
        );
        if (localDup) {
          uploadTagsInput.classList.add("input-error");
          uploadTagsInput.classList.remove("input-valid");
          uploadTip.className = "tag-tip-error";
          uploadTip.innerText = `⚠️ 标签 “${val}” 已被归档 [${localDup.filename}] 占用，标签作为唯一标识不能重复！`;
          uploadTip.style.display = "flex";
          return;
        }

        // 向后端精准核实
        try {
          const res = await this.api(`/api/archives/check-tag?tag=${encodeURIComponent(val)}`);
          if (res.ok) {
            const data = await res.json();
            if (data.exists) {
              uploadTagsInput.classList.add("input-error");
              uploadTagsInput.classList.remove("input-valid");
              uploadTip.className = "tag-tip-error";
              uploadTip.innerText = `⚠️ 标签 “${data.tag || val}” 已被归档 [${data.matched_filename || '已有归档'}] 占用，标签作为唯一标识不能重复！`;
              uploadTip.style.display = "flex";
              return;
            }
          }
        } catch (e) {}

        uploadTagsInput.classList.remove("input-error");
        uploadTagsInput.classList.add("input-valid");
        uploadTip.className = "tag-tip-valid";
        uploadTip.innerText = `✔ 标签 “${val}” 唯一可用`;
        uploadTip.style.display = "flex";
      };

      uploadTagsInput.addEventListener("input", () => {
        clearTimeout(uploadTimer);
        uploadTimer = setTimeout(checkUploadTag, 200);
      });
      uploadTagsInput.addEventListener("blur", checkUploadTag);
    }

    // 编辑弹窗中的标签查重
    const editTagsInput = document.getElementById("archive-meta-tags");
    const editTip = document.getElementById("archive-meta-tag-tip");
    let editTimer = null;

    if (editTagsInput && editTip) {
      const checkEditTag = async () => {
        const val = editTagsInput.value.trim();
        const currentId = document.getElementById("archive-meta-id")?.value;
        if (!val) {
          editTagsInput.classList.add("input-error");
          editTagsInput.classList.remove("input-valid");
          editTip.className = "tag-tip-error";
          editTip.innerText = "⚠️ 归档标签不能为空，标签为归档唯一标识！";
          editTip.style.display = "flex";
          return;
        }

        try {
          const res = await this.api(`/api/archives/check-tag?tag=${encodeURIComponent(val)}&exclude_id=${encodeURIComponent(currentId || "")}`);
          if (res.ok) {
            const data = await res.json();
            if (data.exists) {
              editTagsInput.classList.add("input-error");
              editTagsInput.classList.remove("input-valid");
              editTip.className = "tag-tip-error";
              editTip.innerText = `⚠️ 标签 “${data.tag || val}” 已被归档 [${data.matched_filename || '已有归档'}] 占用，不能重复！`;
              editTip.style.display = "flex";
              return;
            }
          }
        } catch (e) {}

        editTagsInput.classList.remove("input-error");
        editTagsInput.classList.add("input-valid");
        editTip.className = "tag-tip-valid";
        editTip.innerText = `✔ 标签可用`;
        editTip.style.display = "flex";
      };

      editTagsInput.addEventListener("input", () => {
        clearTimeout(editTimer);
        editTimer = setTimeout(checkEditTag, 200);
      });
      editTagsInput.addEventListener("blur", checkEditTag);
    }
  },

  async uploadFile(file) {
    const pBox = document.getElementById("upload-progress-box");
    const pBar = document.getElementById("upload-progress-bar");
    const pPercent = document.getElementById("upload-percent");
    const pName = document.getElementById("upload-filename");

    const onlineWorkers = (this.nodes || []).filter(n => n.role !== "manager" && n.status === "online");
    if (onlineWorkers.length === 0) {
      alert("⚠️ 上传失败：当前集群无任何在线可用的业务存储节点！\n\n日志包只能保存在业务存储节点上，严禁存入管理节点系统盘以避免系统盘被占满。\n请先接入并启动业务节点服务后再上传日志。");
      return;
    }

    const targetNodeSelect = document.getElementById("upload-target-node");
    const targetNodeID = targetNodeSelect ? targetNodeSelect.value : "auto";
    if (!targetNodeID || targetNodeID === "manager_primary" || targetNodeID === "local") {
      alert("⚠️ 上传失败：禁止选择管理节点系统盘存储日志！日志包只能保存在业务存储节点上以避免系统盘被占满。");
      return;
    }

    const tagsInput = document.getElementById("upload-tags");
    const remarkInput = document.getElementById("upload-remark");
    const tagsVal = tagsInput ? tagsInput.value.trim() : "";
    const remarkVal = remarkInput ? remarkInput.value.trim() : "";

    // 1. 强制必填标签校验
    if (!tagsVal) {
      alert("⚠️ 上传被拦截：必须输入归档标签！\n\n日志归档标签为系统唯一标识，请先输入唯一标签后再点击或拖拽上传。");
      tagsInput?.classList.add("input-error");
      tagsInput?.focus();
      const uploadTip = document.getElementById("upload-tag-tip");
      if (uploadTip) {
        uploadTip.className = "tag-tip-error";
        uploadTip.innerText = "⚠️ 归档标签为必填项，请输入唯一标签标识！";
        uploadTip.style.display = "flex";
      }
      return;
    }

    // 2. 本地快速查重校验
    const localDup = (this.archives || []).find(a => 
      (a.tags || []).some(t => t.trim().toLowerCase() === tagsVal.toLowerCase())
    );
    if (localDup) {
      alert(`⚠️ 上传被拦截：标签 “${tagsVal}” 已被现有归档包 [${localDup.filename}] 占用！\n\n归档标签为系统唯一标识，不可重复。请修改为唯一标签后再上传。`);
      tagsInput?.classList.add("input-error");
      tagsInput?.focus();
      return;
    }

    // 3. 服务端权威实时查重校验
    try {
      const checkRes = await this.api(`/api/archives/check-tag?tag=${encodeURIComponent(tagsVal)}`);
      if (checkRes.ok) {
        const checkData = await checkRes.json();
        if (checkData.exists) {
          alert(`⚠️ 上传被拦截：标签 “${checkData.tag || tagsVal}” 已被归档包 [${checkData.matched_filename || '已有归档'}] 占用！\n\n归档标签为系统唯一标识，不可重复。`);
          tagsInput?.classList.add("input-error");
          tagsInput?.focus();
          return;
        }
      }
    } catch (e) {}

    pBox.style.display = "block";
    pName.innerText = `正在上传: ${file.name} (${(file.size / (1024 * 1024)).toFixed(2)} MB)`;
    pBar.style.width = "0%";
    pPercent.innerText = "0%";

    const fd = new FormData();
    fd.append("file", file);
    fd.append("target_node_id", targetNodeID);
    fd.append("tags", tagsVal);
    if (remarkVal) fd.append("remark", remarkVal);

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
        if (tagsInput) {
          tagsInput.value = "";
          tagsInput.classList.remove("input-error", "input-valid");
        }
        if (remarkInput) remarkInput.value = "";
        const uploadTip = document.getElementById("upload-tag-tip");
        if (uploadTip) {
          uploadTip.style.display = "none";
          uploadTip.innerText = "";
        }
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
    const a = (this.archives || []).find(item => item.id === id);
    if (a && a.pinned) {
      alert(`⚠️ 无法直接删除：日志归档包 [${a.filename}] 当前处于“保护锁定”状态！\n\n如需彻底清理删除，请先在操作列点击【🔓 解锁】解除保护后再执行删除。`);
      return;
    }
    const name = a ? a.filename : "该日志包";
    if (!confirm(`确定要彻底删除日志归档包 [${name}] 吗？\n\n注意：此操作将永久清理该归档包及其解压缩产生的所有日志文件、索引文件并释放占用的磁盘空间。`)) return;

    try {
      const res = await this.api(`/api/archives/${id}`, "DELETE");
      if (!res.ok) {
        alert("删除失败: " + (await res.text()));
        return;
      }
      this.loadArchives();
      // 延迟 1 秒后刷新节点列表与用户存储容量，展示释放后的最新磁盘空间
      setTimeout(() => {
        this.loadNodes();
        this.fetchMe();
      }, 1000);
    } catch (e) {
      alert("删除异常: " + e.message);
    }
  },

  async toggleArchivePin(id, pinned) {
    try {
      const res = await this.api("/api/archives/pin", "POST", { archive_id: id, pinned });
      if (!res.ok) {
        alert("切换保护状态失败: " + (await res.text()));
        return;
      }
      this.loadArchives();
    } catch (e) {
      alert("网络异常: " + e.message);
    }
  },

  async openRetentionModal() {
    try {
      const res = await this.api("/api/settings/retention", "GET");
      if (res.ok) {
        const cfg = await res.json();
        document.getElementById("retention-auto-clean").checked = !!cfg.auto_clean_enabled;
        document.getElementById("retention-days").value = (cfg.retention_days !== undefined && cfg.retention_days !== null) ? cfg.retention_days : 180;
        document.getElementById("retention-high-watermark").value = cfg.high_watermark_percent || 85;
        document.getElementById("retention-emergency-watermark").value = cfg.emergency_watermark_percent || 92;
        document.getElementById("retention-target-watermark").value = cfg.target_watermark_percent || 75;
        document.getElementById("retention-exempt-tags").value = (cfg.exempt_tags && Array.isArray(cfg.exempt_tags)) ? cfg.exempt_tags.join(", ") : "";
      }
      this.openModal("modal-retention-settings");
    } catch (e) {
      alert("获取配置失败: " + e.message);
    }
  },

  async saveRetentionSettings(e) {
    e.preventDefault();
    const rawTags = (document.getElementById("retention-exempt-tags").value || "").split(/[,，\n]+/);
    const exemptTags = Array.from(new Set(rawTags.map(t => t.trim()).filter(t => t.length > 0)));

    const payload = {
      auto_clean_enabled: document.getElementById("retention-auto-clean").checked,
      retention_days: parseInt(document.getElementById("retention-days").value, 10) || 0,
      high_watermark_percent: parseInt(document.getElementById("retention-high-watermark").value, 10) || 85,
      emergency_watermark_percent: parseInt(document.getElementById("retention-emergency-watermark").value, 10) || 92,
      target_watermark_percent: parseInt(document.getElementById("retention-target-watermark").value, 10) || 75,
      exempt_tags: exemptTags,
    };
    try {
      const res = await this.api("/api/settings/retention", "PUT", payload);
      if (!res.ok) {
        alert("保存失败: " + (await res.text()));
        return;
      }
      alert("✔ 存储生命周期与磁盘水位自愈配置保存成功！");
      this.closeModal("modal-retention-settings");
    } catch (err) {
      alert("保存异常: " + err.message);
    }
  },

  // ================= 在线文件树与日志查看 =================

  async viewFiles(archiveID) {
    this.currentViewingArchiveID = archiveID;
    this.currentViewingFile = null;
    this.browserFiles = [];
    this.viewerMatches = [];
    this.viewerMatchIndex = -1;
    this.expandedDirs = new Set();
    this.fileTreeRoot = null;
    this.currentTreeFilter = "";

    document.getElementById("viewer-filepath").innerText = "请从左侧选择文件...";
    document.getElementById("viewer-content").innerHTML = "";
    document.getElementById("viewer-meta").innerText = "";
    const dlBtn = document.getElementById("btn-download-current-file");
    if (dlBtn) dlBtn.style.display = "none";
    const statusBadge = document.getElementById("viewer-scroll-status");
    if (statusBadge) statusBadge.style.display = "none";

    this.viewerStream = {
      loadedLines: [],
      baseStartLine: 1,
      renderedStartIdx: 0,
      topSpacerHeight: 0,
      isLoadingMore: false,
      hasMore: false,
      totalLines: 0,
      batchSize: 500,
      maxDomLines: 1200,
      pruneThreshold: 400,
    };
    this.currentFileLines = [];

    const filterInput = document.getElementById("browser-tree-filter");
    if (filterInput) filterInput.value = "";
    const searchKw = document.getElementById("archive-search-keyword");
    if (searchKw) searchKw.value = "";
    const searchSummary = document.getElementById("archive-search-summary");
    if (searchSummary) searchSummary.style.display = "none";
    const searchResults = document.getElementById("archive-search-results");
    if (searchResults) searchResults.innerHTML = '<div style="text-align: center; color: var(--text-dim); padding: 30px 10px; font-size: 12px;">输入关键词并在当前日志压缩包内执行全局搜索</div>';

    this.switchBrowserTab("tree");

    try {
      const res = await this.api(`/api/archives/${archiveID}/files`);
      if (!res.ok) {
        alert(await res.text());
        return;
      }
      const files = await res.json();
      this.browserFiles = (files || []).filter(f => !this.isInternalIndexFile(f.relative_path));
      const fileCountEl = document.getElementById("browser-file-count");
      if (fileCountEl) fileCountEl.innerText = this.browserFiles.filter(f => !f.is_directory).length;

      this.fileTreeRoot = this.buildFileTree(this.browserFiles);
      // 智能层级展开：小于 120 个文件时全展开；海量文件场景下仅展开前两层与目标文件链路，避免几万 DOM 节点卡死浏览器
      if (!this.expandedDirs) this.expandedDirs = new Set();
      this.expandedDirs.clear();

      if (this.browserFiles.length <= 120) {
        this.expandAllTreeDirs(false);
      } else {
        // 展开第一层子目录，其余层级保留折叠状态按需点击展开
        Object.values(this.fileTreeRoot.children || {}).forEach(c => {
          if (c.isDirectory && c.path) {
            this.expandedDirs.add(c.path);
          }
        });
      }

      // 若当前未选中任何文件，默认打开第一个非目录文件，并展开其父目录链路
      const firstFile = this.browserFiles.find(f => !f.is_directory);
      if (firstFile) {
        this.ensureParentDirsExpanded(firstFile.relative_path);
      }

      this.renderFileTree(archiveID, this.browserFiles);

      if (firstFile) {
        this.loadFileContent(archiveID, firstFile.relative_path, firstFile.size);
      }

      // 检查并恢复用户偏好的全屏最大化状态
      const modal = document.getElementById("modal-file-browser");
      if (localStorage.getItem("fileBrowserMaximized") === "true") {
        modal?.classList.add("maximized");
        const icon = document.getElementById("icon-file-browser-maximize");
        const btn = document.getElementById("btn-file-browser-maximize");
        if (icon) icon.innerHTML = "&#x1F5D7;";
        if (btn) btn.title = "还原窗口大小 (支持双击标题栏还原)";
      } else {
        modal?.classList.remove("maximized");
        const icon = document.getElementById("icon-file-browser-maximize");
        const btn = document.getElementById("btn-file-browser-maximize");
        if (icon) icon.innerHTML = "⛶";
        if (btn) btn.title = "最大化占满浏览器 (支持双击标题栏最大化)";
      }

      this.openModal("modal-file-browser");
    } catch (e) {
      alert("获取文件树失败: " + e.message);
    }
  },

  switchBrowserTab(tab) {
    const btnTree = document.getElementById("tab-btn-tree");
    const btnSearch = document.getElementById("tab-btn-search");
    const tabTree = document.getElementById("browser-tab-tree");
    const tabSearch = document.getElementById("browser-tab-search");

    if (tab === "search") {
      btnTree?.classList.remove("active");
      btnSearch?.classList.add("active");
      if (tabTree) tabTree.style.display = "none";
      if (tabSearch) tabSearch.style.display = "flex";
      setTimeout(() => document.getElementById("archive-search-keyword")?.focus(), 50);
    } else {
      btnSearch?.classList.remove("active");
      btnTree?.classList.add("active");
      if (tabSearch) tabSearch.style.display = "none";
      if (tabTree) tabTree.style.display = "flex";
    }
  },

  // 构建目录层级树结构
  buildFileTree(files) {
    const root = {
      name: "",
      path: "",
      isDirectory: true,
      children: {},
      file: null,
      totalFiles: 0,
    };

    (files || []).forEach(f => {
      if (this.isInternalIndexFile(f.relative_path)) return;
      const cleanPath = (f.relative_path || "").replace(/^[./\\]+/, "");
      if (!cleanPath) return;

      const parts = cleanPath.split("/").filter(Boolean);
      let curr = root;
      let accPath = "";

      parts.forEach((part, idx) => {
        accPath = accPath ? `${accPath}/${part}` : part;
        const isLast = idx === parts.length - 1;

        if (!curr.children[part]) {
          curr.children[part] = {
            name: part,
            path: accPath,
            isDirectory: isLast ? !!f.is_directory : true,
            children: {},
            file: isLast && !f.is_directory ? f : null,
            totalFiles: 0,
          };
        } else if (isLast) {
          if (!f.is_directory) {
            curr.children[part].file = f;
            curr.children[part].isDirectory = false;
          }
        }
        curr = curr.children[part];
      });
    });

    function calcTotalFiles(node) {
      if (!node.isDirectory) return 1;
      let sum = 0;
      Object.values(node.children).forEach(child => {
        sum += calcTotalFiles(child);
      });
      node.totalFiles = sum;
      return sum;
    }
    calcTotalFiles(root);

    return root;
  },

  // 确保指定文件的所有父层级目录均已展开
  ensureParentDirsExpanded(filePath) {
    if (!this.expandedDirs) this.expandedDirs = new Set();
    const parts = (filePath || "").split("/").filter(Boolean);
    let acc = "";
    for (let i = 0; i < parts.length - 1; i++) {
      acc = acc ? `${acc}/${parts[i]}` : parts[i];
      this.expandedDirs.add(acc);
    }
  },

  // 全部展开目录树
  expandAllTreeDirs(doRender = true) {
    if (!this.expandedDirs) this.expandedDirs = new Set();
    if (!this.fileTreeRoot) return;
    const addDirs = (node) => {
      if (node.isDirectory && node.path) {
        this.expandedDirs.add(node.path);
      }
      Object.values(node.children || {}).forEach(addDirs);
    };
    addDirs(this.fileTreeRoot);
    if (doRender) {
      this.renderFileTree(this.currentViewingArchiveID, this.browserFiles, this.currentTreeFilter);
    }
  },

  // 全部折叠目录树
  collapseAllTreeDirs() {
    if (!this.expandedDirs) this.expandedDirs = new Set();
    this.expandedDirs.clear();
    if (this.currentViewingFile && this.currentViewingFile.relPath) {
      this.ensureParentDirsExpanded(this.currentViewingFile.relPath);
    }
    this.renderFileTree(this.currentViewingArchiveID, this.browserFiles, this.currentTreeFilter);
  },

  // 点击切换单个目录展开/折叠
  toggleDir(archiveID, dirPath) {
    if (!this.expandedDirs) this.expandedDirs = new Set();
    if (this.expandedDirs.has(dirPath)) {
      this.expandedDirs.delete(dirPath);
    } else {
      this.expandedDirs.add(dirPath);
    }
    this.renderFileTree(archiveID, this.browserFiles, this.currentTreeFilter);
  },

  // 过滤目录树（支持文件名和目录名匹配，匹配项父路径自动展开）
  filterFileTree(query) {
    this.currentTreeFilter = (query || "").trim();
    this.renderFileTree(this.currentViewingArchiveID, this.browserFiles, this.currentTreeFilter);
  },

  // 针对搜索关键字剪枝树结构
  pruneTree(node, query) {
    const q = (query || "").toLowerCase();
    const nameMatches = node.name.toLowerCase().includes(q) || (node.path && node.path.toLowerCase().includes(q));

    if (!node.isDirectory) {
      return nameMatches ? node : null;
    }

    const prunedChildren = {};
    let hasMatchingChild = false;

    Object.entries(node.children).forEach(([key, child]) => {
      const pruned = this.pruneTree(child, query);
      if (pruned) {
        prunedChildren[key] = pruned;
        hasMatchingChild = true;
      }
    });

    if (nameMatches || hasMatchingChild) {
      if (node.path) {
        this.expandedDirs.add(node.path);
      }
      return {
        ...node,
        children: prunedChildren
      };
    }
    return null;
  },

  // 关键字高亮渲染辅助函数 (先精确按正则分段，再独立转义，杜绝破坏 HTML 实体与 XSS 注入)
  highlightMatch(rawText, query) {
    if (!rawText) return "";
    if (!query || !query.trim()) return this.escape(rawText);
    const q = query.trim();
    const reg = new RegExp(this.escapeRegex(q), "gi");
    let result = "";
    let lastIndex = 0;
    let match;
    while ((match = reg.exec(rawText)) !== null) {
      result += this.escape(rawText.substring(lastIndex, match.index));
      result += `<mark class="v-match">${this.escape(match[0])}</mark>`;
      lastIndex = reg.lastIndex;
    }
    result += this.escape(rawText.substring(lastIndex));
    return result;
  },

  // 渲染层级目录树
  renderFileTree(archiveID, files, filterQuery = "") {
    const container = document.getElementById("file-tree-container");
    if (!container) return;
    container.innerHTML = "";

    if (!files || files.length === 0) {
      container.innerHTML = `<div style="padding: 16px; color: var(--text-dim); text-align: center; font-size: 12px;">无文件列表</div>`;
      return;
    }

    if (!this.fileTreeRoot) {
      this.fileTreeRoot = this.buildFileTree(files);
    }

    let displayRoot = this.fileTreeRoot;
    if (filterQuery) {
      displayRoot = this.pruneTree(this.fileTreeRoot, filterQuery);
      if (!displayRoot || Object.keys(displayRoot.children).length === 0) {
        container.innerHTML = `<div style="padding: 16px; color: var(--text-dim); text-align: center; font-size: 12px;">无匹配文件或目录</div>`;
        return;
      }
    }

    const sortedChildren = Object.values(displayRoot.children).sort((a, b) => {
      if (a.isDirectory !== b.isDirectory) {
        return a.isDirectory ? -1 : 1;
      }
      return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' });
    });

    sortedChildren.forEach(child => {
      const el = this.renderTreeNode(archiveID, child, 0, filterQuery);
      if (el) container.appendChild(el);
    });
  },

  // 递归渲染目录或文件树节点
  renderTreeNode(archiveID, node, depth, filterQuery) {
    if (node.isDirectory) {
      const isExpanded = this.expandedDirs.has(node.path);
      const folderWrapper = document.createElement("div");
      folderWrapper.className = "file-tree-group";

      const row = document.createElement("div");
      row.className = `file-tree-item file-tree-folder ${isExpanded ? 'expanded' : ''}`;
      row.style.paddingLeft = `${8 + depth * 14}px`;

      const arrow = `<span class="tree-arrow">${isExpanded ? '▼' : '▶'}</span>`;
      const icon = `<span class="tree-icon">${isExpanded ? '📂' : '📁'}</span>`;
      const nameHtml = this.highlightMatch(node.name, filterQuery);
      const countBadge = `<span class="tree-badge">${node.totalFiles}</span>`;

      row.innerHTML = `${arrow}${icon}<span class="file-tree-name" title="${this.escape(node.path)}">${nameHtml}</span>${countBadge}`;

      row.addEventListener("click", (e) => {
        e.stopPropagation();
        this.toggleDir(archiveID, node.path);
      });

      folderWrapper.appendChild(row);

      if (isExpanded) {
        const childrenContainer = document.createElement("div");
        childrenContainer.className = "file-tree-children";

        const sortedChildren = Object.values(node.children).sort((a, b) => {
          if (a.isDirectory !== b.isDirectory) {
            return a.isDirectory ? -1 : 1;
          }
          return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' });
        });

        sortedChildren.forEach(child => {
          const childEl = this.renderTreeNode(archiveID, child, depth + 1, filterQuery);
          if (childEl) {
            childrenContainer.appendChild(childEl);
          }
        });
        folderWrapper.appendChild(childrenContainer);
      }

      return folderWrapper;
    } else {
      const fileRow = document.createElement("div");
      fileRow.className = "file-tree-item file-tree-file";
      fileRow.style.paddingLeft = `${8 + depth * 14}px`;
      if (this.currentViewingFile && this.currentViewingFile.relPath === node.path) {
        fileRow.classList.add("active");
      }
      fileRow.dataset.path = node.path;

      const spacer = `<span class="tree-spacer"></span>`;
      const ext = node.name.split('.').pop().toLowerCase();
      let icon = "📄";
      if (["conf", "cfg", "ini", "yaml", "yml", "json", "toml", "xml"].includes(ext)) {
        icon = "⚙️";
      } else if (["log", "txt", "out", "err"].includes(ext)) {
        icon = "📝";
      } else if (["tar", "gz", "zip", "bz2", "xz"].includes(ext)) {
        icon = "📦";
      }

      const nameHtml = this.highlightMatch(node.name, filterQuery);
      const f = node.file;
      const sizeStr = f && f.size > 0 ? (f.size > 1024 * 1024 ? `${(f.size / (1024 * 1024)).toFixed(1)} MB` : `${(f.size / 1024).toFixed(1)} KB`) : '';
      const sizeBadge = sizeStr ? `<span class="tree-size" style="font-size: 10px; color: var(--text-dim); margin-right: 2px;">${sizeStr}</span>` : '';

      const downloadBtn = f ? `<button class="btn-tree-download" title="下载此文件 (${sizeStr})" onclick="event.stopPropagation(); app.downloadFile('${archiveID}', '${this.escape(node.path)}')">📥</button>` : '';

      fileRow.innerHTML = `${spacer}<span class="tree-icon">${icon}</span><span class="file-tree-name" title="${this.escape(node.path)}">${nameHtml}</span>${sizeBadge}${downloadBtn}`;

      fileRow.addEventListener("click", () => {
        document.querySelectorAll(".file-tree-file").forEach(el => el.classList.remove("active"));
        fileRow.classList.add("active");
        this.loadFileContent(archiveID, node.path, f ? f.size : 0);
      });

      return fileRow;
    }
  },

  // 压缩包内全局搜索
  // 压缩包内全局搜索 (按文件分组展示，支持点击具体文件查看该文件完整检索结果)
  async searchInsideArchive() {
    const kwInput = document.getElementById("archive-search-keyword");
    const keyword = (kwInput ? kwInput.value : "").trim();
    if (!keyword) {
      alert("请输入要检索的关键词");
      return;
    }

    const archiveID = this.currentViewingArchiveID;
    if (!archiveID) return;

    const level = document.getElementById("archive-search-level")?.value || "ALL";
    const isRegex = !!document.getElementById("archive-search-regex")?.checked;
    const caseSensitive = !!document.getElementById("archive-search-case")?.checked;
    const wholeWord = !!document.getElementById("archive-search-whole-word")?.checked;

    const resultsBox = document.getElementById("archive-search-results");
    const summaryBox = document.getElementById("archive-search-summary");
    const summaryText = document.getElementById("archive-search-summary-text");

    resultsBox.innerHTML = '<div style="text-align: center; color: var(--text-dim); padding: 30px 10px; font-size: 12px;">正在全包检索并聚合文件统计...</div>';
    if (summaryBox) summaryBox.style.display = "none";

    try {
      const res = await this.api("/api/search", "POST", {
        archive_id: archiveID,
        keyword,
        level,
        is_regex: isRegex,
        case_sensitive: caseSensitive,
        whole_word: wholeWord,
        context_lines: 1,
        page: 1,
        page_size: 500, // 扩大单次拉取量
      });

      if (!res.ok) {
        throw new Error(await res.text());
      }

      const data = await res.json();
      const hits = data.hits || [];
      const totalHits = data.total_hits || 0;

      // 保存当前包内搜索上下文，方便单文件“加载更多”
      this.archiveSearchContext = {
        archiveID,
        keyword,
        level,
        isRegex,
        caseSensitive,
        wholeWord,
        pageSize: 200,
      };

      // 按文件聚合初次返回的 hits
      const hitsByFile = {};
      hits.forEach(h => {
        if (!hitsByFile[h.file_path]) {
          hitsByFile[h.file_path] = [];
        }
        hitsByFile[h.file_path].push(h);
      });

      // 获取文件概要列表 (优先使用后端聚合的 file_summaries，保底提取 hits)
      let fileSummaries = data.file_summaries || [];
      if (fileSummaries.length === 0) {
        fileSummaries = Object.keys(hitsByFile).map(fp => ({
          file_path: fp,
          total_hits: hitsByFile[fp].length,
          max_level: this.detectHitsMaxLevel(hitsByFile[fp]),
        }));
      }

      if (summaryBox && summaryText) {
        summaryBox.style.display = "flex";
        summaryText.innerText = `在 ${fileSummaries.length} 个文件中匹配到 ${totalHits} 处 (耗时 ${data.cost_ms} ms)`;
      }

      if (fileSummaries.length === 0 || totalHits === 0) {
        resultsBox.innerHTML = '<div style="text-align: center; color: var(--text-dim); padding: 30px 10px; font-size: 12px;">未匹配到符合条件的日志行</div>';
        return;
      }

      resultsBox.innerHTML = "";
      fileSummaries.forEach((fs, idx) => {
        const fileHits = hitsByFile[fs.file_path] || [];
        const groupEl = this.createArchiveFileGroupElement(fs, fileHits, idx === 0);
        resultsBox.appendChild(groupEl);
      });
    } catch (e) {
      resultsBox.innerHTML = `<div style="text-align: center; color: var(--danger); padding: 20px 10px; font-size: 12px;">检索异常: ${e.message}</div>`;
    }
  },

  // 构造单个文件的搜索结果手风琴分组
  createArchiveFileGroupElement(fileSummary, initialHits, defaultExpanded = false) {
    const group = document.createElement("div");
    group.className = "archive-file-group" + (defaultExpanded ? " expanded" : "");
    group.dataset.filePath = fileSummary.file_path;
    group.dataset.totalHits = fileSummary.total_hits;
    group.dataset.loadedHits = initialHits.length;

    const fileName = fileSummary.file_path.split("/").pop();
    const dirPath = fileSummary.file_path.includes("/")
      ? fileSummary.file_path.substring(0, fileSummary.file_path.lastIndexOf("/"))
      : "";

    let levelBadge = "";
    if (fileSummary.max_level === "CRITICAL" || fileSummary.max_level === "FATAL") {
      levelBadge = '<span class="badge badge-danger" style="font-size: 9px; padding: 1px 4px;">CRITICAL</span>';
    } else if (fileSummary.max_level === "ERROR") {
      levelBadge = '<span class="badge badge-danger" style="font-size: 9px; padding: 1px 4px;">ERROR</span>';
    } else if (fileSummary.max_level === "WARN" || fileSummary.max_level === "WARNING") {
      levelBadge = '<span class="badge badge-warning" style="font-size: 9px; padding: 1px 4px;">WARN</span>';
    }

    group.innerHTML = `
      <div class="archive-file-group-header" title="${this.escape(fileSummary.file_path)}">
        <div class="archive-file-group-title">
          <span class="archive-file-group-toggle">▶</span>
          <span style="font-size: 13px;">📄</span>
          <span style="font-weight: 500;">${this.escape(fileName)}</span>
          ${dirPath ? `<span style="color: var(--text-dim); font-size: 11px; margin-left: 2px;">(${this.escape(dirPath)})</span>` : ''}
        </div>
        <div class="archive-file-group-badges">
          ${levelBadge}
          <span class="badge badge-info" style="font-size: 10px; padding: 1px 6px;">${fileSummary.total_hits} 处匹配</span>
        </div>
      </div>
      <div class="archive-file-group-body">
        <div class="archive-file-group-hits"></div>
        <div class="archive-file-group-loadmore" style="display: none;"></div>
      </div>
    `;

    const header = group.querySelector(".archive-file-group-header");
    const bodyHits = group.querySelector(".archive-file-group-hits");
    const loadMoreBtn = group.querySelector(".archive-file-group-loadmore");

    // 渲染已有命中行
    if (initialHits.length > 0) {
      initialHits.forEach(h => {
        bodyHits.appendChild(this.createArchiveHitItem(h));
      });
    }

    // 检查是否需要“加载更多”按钮
    this.updateFileGroupLoadMore(group, fileSummary.total_hits, initialHits.length);

    // 点击文件头部切换展开/折叠，并在展开且无结果时按需拉取
    header.addEventListener("click", async () => {
      const isExpanded = group.classList.toggle("expanded");
      if (isExpanded) {
        const loadedCount = parseInt(group.dataset.loadedHits || "0", 10);
        if (loadedCount === 0 && fileSummary.total_hits > 0) {
          await this.loadFileGroupHits(group, fileSummary.file_path, 1);
        }
      }
    });

    // 点击“加载更多”
    loadMoreBtn.addEventListener("click", async (e) => {
      e.stopPropagation();
      const loadedCount = parseInt(group.dataset.loadedHits || "0", 10);
      const nextPage = Math.floor(loadedCount / this.archiveSearchContext.pageSize) + 1;
      await this.loadFileGroupHits(group, fileSummary.file_path, nextPage);
    });

    return group;
  },

  // 渲染单个搜索命中条目
  createArchiveHitItem(h) {
    const item = document.createElement("div");
    item.className = "archive-hit-item";

    const keyword = this.archiveSearchContext?.keyword || "";
    const caseSensitive = this.archiveSearchContext?.caseSensitive || false;
    const isRegex = this.archiveSearchContext?.isRegex || false;
    const wholeWord = this.archiveSearchContext?.wholeWord || false;

    let highlightedSnippet = this.escape(h.content || "");
    if (keyword) {
      let pattern = isRegex ? keyword : this.escapeRegex(keyword);
      if (wholeWord) {
        pattern = `\\b(?:${pattern})\\b`;
      }
      try {
        const reg = new RegExp(`(${pattern})`, caseSensitive ? "g" : "gi");
        highlightedSnippet = highlightedSnippet.replace(reg, '<mark class="v-match">$1</mark>');
      } catch (e) {}
    }

    const levelBadge = h.level === "ERROR" || h.level === "FATAL" || h.level === "CRITICAL"
      ? '<span class="badge badge-danger" style="font-size: 9px; padding: 1px 4px;">' + h.level + '</span>'
      : (h.level === "WARN" ? '<span class="badge badge-warning" style="font-size: 9px; padding: 1px 4px;">WARN</span>' : '');

    item.innerHTML = `
      <div class="archive-hit-header">
        <span style="font-weight: 500; color: var(--primary);">第 ${h.line_number} 行</span>
        <div style="display: flex; gap: 6px; align-items: center;">
          ${h.timestamp ? `<span style="color: var(--text-dim); font-size: 10px;">${this.escape(h.timestamp)}</span>` : ''}
          ${levelBadge}
        </div>
      </div>
      <div class="archive-hit-snippet" title="${this.escape(h.content)}">${highlightedSnippet}</div>
    `;

    item.addEventListener("click", (e) => {
      e.stopPropagation();
      const archiveID = this.currentViewingArchiveID;
      this.ensureParentDirsExpanded(h.file_path);
      this.renderFileTree(archiveID, this.browserFiles, this.currentTreeFilter);
      this.loadFileContent(archiveID, h.file_path, 0, h.line_number, keyword);
    });

    return item;
  },

  // 按需拉取指定文件的匹配项
  async loadFileGroupHits(group, filePath, page) {
    if (!this.archiveSearchContext) return;
    const bodyHits = group.querySelector(".archive-file-group-hits");
    const loadMoreBtn = group.querySelector(".archive-file-group-loadmore");

    if (page === 1) {
      bodyHits.innerHTML = '<div style="text-align: center; color: var(--text-dim); padding: 15px; font-size: 11px;">正在加载文件匹配...</div>';
    } else {
      loadMoreBtn.innerText = "加载中...";
    }

    try {
      const res = await this.api("/api/search", "POST", {
        archive_id: this.archiveSearchContext.archiveID,
        file_path: filePath,
        keyword: this.archiveSearchContext.keyword,
        level: this.archiveSearchContext.level,
        is_regex: this.archiveSearchContext.isRegex,
        case_sensitive: this.archiveSearchContext.caseSensitive,
        whole_word: this.archiveSearchContext.wholeWord,
        context_lines: 1,
        page: page,
        page_size: this.archiveSearchContext.pageSize,
      });

      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      const newHits = data.hits || [];

      if (page === 1) {
        bodyHits.innerHTML = "";
      }

      newHits.forEach(h => {
        bodyHits.appendChild(this.createArchiveHitItem(h));
      });

      const prevCount = page === 1 ? 0 : parseInt(group.dataset.loadedHits || "0", 10);
      const totalLoaded = prevCount + newHits.length;
      group.dataset.loadedHits = totalLoaded;
      group.dataset.totalHits = data.total_hits || group.dataset.totalHits;

      this.updateFileGroupLoadMore(group, parseInt(group.dataset.totalHits, 10), totalLoaded);
    } catch (err) {
      if (page === 1) {
        bodyHits.innerHTML = `<div style="text-align: center; color: var(--danger); padding: 10px; font-size: 11px;">加载失败: ${err.message}</div>`;
      } else {
        loadMoreBtn.innerText = "加载失败，点击重试";
      }
    }
  },

  // 更新“加载更多”按钮状态
  updateFileGroupLoadMore(group, totalHits, loadedHits) {
    const loadMoreBtn = group.querySelector(".archive-file-group-loadmore");
    if (!loadMoreBtn) return;
    if (loadedHits < totalHits) {
      loadMoreBtn.style.display = "block";
      loadMoreBtn.innerText = `⬇️ 加载更多本文件匹配 (已显示 ${loadedHits} / 共 ${totalHits} 条)`;
    } else {
      loadMoreBtn.style.display = "none";
    }
  },

  // 批量展开或折叠全部文件分组
  toggleAllArchiveFileGroups(expand) {
    const groups = document.querySelectorAll(".archive-file-group");
    groups.forEach(g => {
      if (expand) {
        g.classList.add("expanded");
        const loadedCount = parseInt(g.dataset.loadedHits || "0", 10);
        const totalHits = parseInt(g.dataset.totalHits || "0", 10);
        const filePath = g.dataset.filePath;
        if (loadedCount === 0 && totalHits > 0 && filePath) {
          this.loadFileGroupHits(g, filePath, 1);
        }
      } else {
        g.classList.remove("expanded");
      }
    });
  },

  detectHitsMaxLevel(hits) {
    let hasErr = false;
    let hasWarn = false;
    for (const h of hits) {
      const lvl = (h.level || "").toUpperCase();
      if (lvl === "CRITICAL" || lvl === "FATAL") return "CRITICAL";
      if (lvl === "ERROR") hasErr = true;
      if (lvl === "WARN" || lvl === "WARNING") hasWarn = true;
    }
    if (hasErr) return "ERROR";
    if (hasWarn) return "WARN";
    return "INFO";
  },

  // ================= 极速流式日志查看器 (无限下拉触底加载 + DOM滑动窗口防爆内存) =================

  // 加载具体文件内容 (支持跳转至指定行及高亮搜索词)
  async loadFileContent(archiveID, relPath, size = 0, targetLine = 0, highlightKeyword = "", startLine = 1, limit = 500) {
    if (targetLine > 0) {
      startLine = Math.max(1, targetLine - 50);
      limit = Math.max(500, targetLine - startLine + 100);
      this.viewerTargetLine = targetLine;
    }

    // 若切换至全新文件且未携带搜索高亮，重置搜索输入框、过滤模式和命中集
    const isDifferentFile = !this.currentViewingFile || this.currentViewingFile.archiveID !== archiveID || this.currentViewingFile.relPath !== relPath;
    if (isDifferentFile && !highlightKeyword) {
      this.viewerFileSearchHits = [];
      this.viewerFileSearchIndex = -1;
      this.viewerIsFilterMode = false;
      const kwInput = document.getElementById("viewer-search-kw");
      if (kwInput) kwInput.value = "";
      const filterCheckbox = document.getElementById("viewer-search-filter-mode");
      if (filterCheckbox) filterCheckbox.checked = false;
      const counter = document.getElementById("viewer-search-counter");
      if (counter) counter.innerText = "0 / 0";
    }

    this.currentViewingFile = { archiveID, relPath, size };
    this.viewerMatches = [];
    this.viewerMatchIndex = -1;

    // 初始化流式与滑动窗口状态
    this.viewerStream = {
      loadedLines: [],          // 所有已加载的行纯文本
      baseStartLine: startLine, // loadedLines[0] 对应的实际文件行号
      renderedStartIdx: 0,      // DOM 当前第一个子节点在 loadedLines 中的下标
      topSpacerHeight: 0,       // 顶部被剪枝行的高度占位 (px)
      isLoadingMore: false,
      hasMore: false,
      totalLines: 0,
      batchSize: 500,
      maxDomLines: 1200,        // DOM 容纳的最大行数，超过则剪枝顶部
      pruneThreshold: 400,      // 每次剪枝或恢复的批次大小
    };
    this.currentFileLines = [];

    const viewer = document.getElementById("viewer-content");
    viewer.innerHTML = `
      <div id="v-top-spacer" style="height: 0px; width: 100%; flex-shrink: 0;"></div>
      <div id="v-lines-container" style="display: flex; flex-direction: column; width: 100%;"></div>
      <div id="v-bottom-sentinel" class="v-sentinel">
        <span class="spinner-border spinner-border-sm" style="margin-right: 6px;"></span>正在读取日志文件内容...
      </div>
    `;
    viewer.scrollTop = 0;

    document.getElementById("viewer-filepath").innerText = relPath;
    document.getElementById("viewer-meta").innerText = size > 0 ? `大小: ${(size / 1024).toFixed(1)} KB` : "";

    const statusBadge = document.getElementById("viewer-scroll-status");
    if (statusBadge) {
      statusBadge.style.display = "inline-flex";
      statusBadge.className = "badge badge-info";
      statusBadge.innerText = "流式载入中...";
    }

    const dlBtn = document.getElementById("btn-download-current-file");
    if (dlBtn) {
      const fileName = relPath.split("/").pop() || "log.txt";
      dlBtn.style.display = "inline-flex";
      dlBtn.innerHTML = `📥 下载此文件 (${this.escape(fileName)})`;
    }

    try {
      const res = await this.api(`/api/archives/${archiveID}/file-content?path=${encodeURIComponent(relPath)}&start_line=${startLine}&limit=${limit}`);
      if (!res.ok) {
        const sentinel = document.getElementById("v-bottom-sentinel");
        if (sentinel) sentinel.innerHTML = `<span style="color: var(--danger);">读取文件失败: ${await res.text()}</span>`;
        if (statusBadge) statusBadge.style.display = "none";
        return;
      }
      const data = await res.json();
      const lines = data.lines || [];
      this.viewerStream.loadedLines = lines;
      this.viewerStream.hasMore = !!data.has_more;
      this.currentFileLines = lines;

      // 提取文件总行数
      const fileItem = this.browserFiles ? this.browserFiles.find(f => f.relative_path === relPath) : null;
      this.viewerStream.totalLines = (fileItem && fileItem.line_count > 0) ? fileItem.line_count : (data.total_lines || 0);

      const container = document.getElementById("v-lines-container");
      if (lines.length === 0) {
        const sentinel = document.getElementById("v-bottom-sentinel");
        if (sentinel) sentinel.innerHTML = `<span style="color: var(--text-dim);">该日志文件为空</span>`;
        if (statusBadge) statusBadge.style.display = "none";
        return;
      }

      const fragment = document.createDocumentFragment();
      for (let i = 0; i < lines.length; i++) {
        const lineNo = this.viewerStream.baseStartLine + i;
        const lineDiv = this.createLineElement(lineNo, lines[i], targetLine);
        fragment.appendChild(lineDiv);
      }
      container.appendChild(fragment);

      this.updateViewerStatusUI();

      // 更新左侧目录树中的高亮选中状态与视口定位
      document.querySelectorAll(".file-tree-file").forEach(el => {
        if (el.dataset.path === relPath) {
          el.classList.add("active");
          el.scrollIntoView({ block: "nearest", behavior: "smooth" });
        } else {
          el.classList.remove("active");
        }
      });

      // 如果指定了目标行，平滑滚动定位
      if (targetLine > 0) {
        setTimeout(() => {
          const targetEl = document.getElementById(`v-line-${targetLine}`);
          if (targetEl) {
            targetEl.scrollIntoView({ block: "center", behavior: "smooth" });
          }
        }, 60);
      }

      // 如果有指定的高亮关键词，自动填充至文件内搜索栏并触发匹配
      if (highlightKeyword) {
        const searchInput = document.getElementById("viewer-search-kw");
        if (searchInput && searchInput.value !== highlightKeyword) {
          searchInput.value = highlightKeyword;
          this.onViewerSearchInput();
        } else if (searchInput && searchInput.value === highlightKeyword) {
          const isCase = !!document.getElementById("viewer-search-case")?.checked;
          const isRegex = !!document.getElementById("viewer-search-regex")?.checked;
          const isWholeWord = !!document.getElementById("viewer-search-whole-word")?.checked;
          this.applyViewerSearchHighlights(highlightKeyword, isCase, isRegex, isWholeWord);
          if (targetLine > 0) {
            const targetEl = document.getElementById(`v-line-${targetLine}`);
            if (targetEl) {
              const markEl = targetEl.querySelector("mark.v-match");
              if (markEl) markEl.classList.add("v-match-active");
            }
          }
        }
      }
    } catch (e) {
      const sentinel = document.getElementById("v-bottom-sentinel");
      if (sentinel) sentinel.innerHTML = `<span style="color: var(--danger);">读取异常: ${e.message}</span>`;
      if (statusBadge) statusBadge.style.display = "none";
    }
  },

  // 创建单行 DOM 节点
  createLineElement(lineNo, text, highlightLine = 0) {
    const lineDiv = document.createElement("div");
    lineDiv.className = "v-line";
    lineDiv.id = `v-line-${lineNo}`;
    lineDiv.dataset.line = lineNo;
    if (highlightLine > 0 && lineNo === highlightLine) {
      lineDiv.classList.add("v-line-highlight");
    }

    let textHtml = this.escape(text);
    if (!this.viewerIsFilterMode) {
      const searchInput = document.getElementById("viewer-search-kw");
      const keyword = (searchInput ? searchInput.value : "").trim();
      if (keyword) {
        const isCase = !!document.getElementById("viewer-search-case")?.checked;
        const isRegex = !!document.getElementById("viewer-search-regex")?.checked;
        try {
          const pattern = isRegex ? keyword : this.escapeRegex(keyword);
          const reg = new RegExp(`(${pattern})`, isCase ? "g" : "gi");
          textHtml = textHtml.replace(reg, '<mark class="v-match">$1</mark>');
        } catch (e) {}
      }
    }

    lineDiv.innerHTML = `<span class="v-line-no">${lineNo}</span><span class="v-line-text">${textHtml}</span>`;
    return lineDiv;
  },

  // 更新查看器状态栏信息（已载入行数统计与触底流式状态）
  updateViewerStatusUI() {
    if (!this.viewerStream) return;
    const { baseStartLine, loadedLines, hasMore, totalLines, renderedStartIdx } = this.viewerStream;
    const container = document.getElementById("v-lines-container");
    const domCount = container ? container.children.length : 0;
    const startNo = baseStartLine + renderedStartIdx;
    const endNo = domCount > 0 ? (startNo + domCount - 1) : startNo;
    const totalLoaded = loadedLines.length;

    const totalStr = totalLines > 0 ? ` (共 ${totalLines.toLocaleString()} 行)` : '';
    const metaEl = document.getElementById("viewer-meta");
    if (metaEl) {
      metaEl.innerText = `已载入 ${totalLoaded.toLocaleString()} 行 · 视口第 ${startNo.toLocaleString()} ~ ${endNo.toLocaleString()} 行${totalStr}`;
    }

    const statusBadge = document.getElementById("viewer-scroll-status");
    const sentinel = document.getElementById("v-bottom-sentinel");

    if (!hasMore) {
      if (statusBadge) {
        statusBadge.style.display = "inline-flex";
        statusBadge.className = "badge badge-success";
        statusBadge.innerText = "已加载全部内容";
      }
      if (sentinel) {
        sentinel.innerHTML = `<span style="color: var(--text-dim); font-size: 11px;">✔ 已到达尾部，全部日志已就绪 (共 ${totalLoaded.toLocaleString()} 行)</span>`;
      }
    } else {
      if (statusBadge) {
        statusBadge.style.display = "inline-flex";
        statusBadge.className = "badge badge-primary";
        statusBadge.innerText = "下拉自动加载更多";
      }
      if (sentinel) {
        sentinel.innerHTML = `<span style="color: var(--text-muted); font-size: 11px;">⬇ 下拉至尾部自动加载更多日志...</span>`;
      }
    }
  },

  // 滚动事件：触底自动加载 + 滑动窗口 DOM 剪枝与恢复 (杜绝内存占满)
  onViewerScroll() {
    const viewer = document.getElementById("viewer-content");
    if (!viewer || !this.viewerStream || !this.currentViewingFile) return;

    const { scrollTop, clientHeight, scrollHeight } = viewer;

    // 1. 触底检测与自动拉取下一批日志
    if (scrollHeight - (scrollTop + clientHeight) < 350) {
      if (!this.viewerStream.isLoadingMore && this.viewerStream.hasMore) {
        this.loadNextBatchLines();
      }
    }

    // 2. 向下滚动过多时的 DOM 剪枝（严格限制 DOM 节点总数 <= 1200，杜绝 DOM 膨胀占用内存）
    const container = document.getElementById("v-lines-container");
    if (container && container.children.length > this.viewerStream.maxDomLines) {
      this.pruneTopDomLines();
    }

    // 3. 向上滚动接近顶部时的 DOM 恢复（无缝回填已剪枝行）
    if (scrollTop < 250 && this.viewerStream.renderedStartIdx > 0) {
      this.restoreTopDomLines();
    }
  },

  // 加载下一批日志行
  async loadNextBatchLines() {
    if (!this.viewerStream || this.viewerStream.isLoadingMore || !this.viewerStream.hasMore) return;
    this.viewerStream.isLoadingMore = true;

    const sentinel = document.getElementById("v-bottom-sentinel");
    if (sentinel) {
      sentinel.innerHTML = `<span class="spinner-border spinner-border-sm" style="margin-right: 6px;"></span>正在自动加载更多日志...`;
    }

    const nextStartLine = this.viewerStream.baseStartLine + this.viewerStream.loadedLines.length;
    const limit = this.viewerStream.batchSize;

    try {
      const res = await this.api(`/api/archives/${this.currentViewingFile.archiveID}/file-content?path=${encodeURIComponent(this.currentViewingFile.relPath)}&start_line=${nextStartLine}&limit=${limit}`);
      if (!res.ok) {
        if (sentinel) sentinel.innerHTML = `<span style="color: var(--danger);">加载失败: ${await res.text()}</span>`;
        return;
      }
      const data = await res.json();
      const newLines = data.lines || [];
      this.viewerStream.hasMore = !!data.has_more;

      if (newLines.length > 0) {
        const container = document.getElementById("v-lines-container");
        const fragment = document.createDocumentFragment();

        for (let i = 0; i < newLines.length; i++) {
          const lineNo = nextStartLine + i;
          const lineDiv = this.createLineElement(lineNo, newLines[i], 0);
          fragment.appendChild(lineDiv);
        }
        container.appendChild(fragment);

        this.viewerStream.loadedLines = this.viewerStream.loadedLines.concat(newLines);
        this.currentFileLines = this.viewerStream.loadedLines;

        // 剪枝检查
        if (container.children.length > this.viewerStream.maxDomLines) {
          this.pruneTopDomLines();
        }

        this.updateViewerStatusUI();

        // 若有活跃搜索词，更新高亮
        if (document.getElementById("viewer-search-kw")?.value) {
          this.onViewerSearchInput();
        }
      } else {
        this.viewerStream.hasMore = false;
        this.updateViewerStatusUI();
      }
    } catch (e) {
      if (sentinel) sentinel.innerHTML = `<span style="color: var(--danger);">自动加载异常: ${e.message}</span>`;
    } finally {
      this.viewerStream.isLoadingMore = false;
    }
  },

  // 剪除顶部多余 DOM 节点并增加占位垫片高度（内存保护）
  pruneTopDomLines() {
    const container = document.getElementById("v-lines-container");
    const topSpacer = document.getElementById("v-top-spacer");
    const viewer = document.getElementById("viewer-content");
    if (!container || !topSpacer || !viewer) return;

    const pruneCount = Math.min(this.viewerStream.pruneThreshold, container.children.length - 400);
    if (pruneCount <= 0) return;

    let removedHeight = 0;
    for (let i = 0; i < pruneCount; i++) {
      const child = container.children[i];
      removedHeight += child.offsetHeight;
    }

    for (let i = 0; i < pruneCount; i++) {
      container.removeChild(container.firstChild);
    }

    this.viewerStream.renderedStartIdx += pruneCount;
    this.viewerStream.topSpacerHeight += removedHeight;
    topSpacer.style.height = `${this.viewerStream.topSpacerHeight}px`;

    this.updateViewerStatusUI();
  },

  // 向上滚动时恢复顶部剪除的 DOM 节点
  restoreTopDomLines() {
    const container = document.getElementById("v-lines-container");
    const topSpacer = document.getElementById("v-top-spacer");
    const viewer = document.getElementById("viewer-content");
    if (!container || !topSpacer || !viewer || this.viewerStream.renderedStartIdx <= 0) return;

    const restoreCount = Math.min(this.viewerStream.pruneThreshold, this.viewerStream.renderedStartIdx);
    if (restoreCount <= 0) return;

    const newStartIdx = this.viewerStream.renderedStartIdx - restoreCount;
    const fragment = document.createDocumentFragment();

    for (let i = 0; i < restoreCount; i++) {
      const idx = newStartIdx + i;
      const lineNo = this.viewerStream.baseStartLine + idx;
      const text = this.viewerStream.loadedLines[idx];
      const lineDiv = this.createLineElement(lineNo, text, 0);
      fragment.appendChild(lineDiv);
    }

    container.insertBefore(fragment, container.firstChild);

    let restoredHeight = 0;
    for (let i = 0; i < restoreCount; i++) {
      restoredHeight += container.children[i].offsetHeight;
    }

    this.viewerStream.renderedStartIdx = newStartIdx;
    this.viewerStream.topSpacerHeight = Math.max(0, this.viewerStream.topSpacerHeight - restoredHeight);
    topSpacer.style.height = `${this.viewerStream.topSpacerHeight}px`;

    // 补偿滚动高度，视觉上无任何闪烁跳跃
    viewer.scrollTop += restoredHeight;

    this.updateViewerStatusUI();

    // 如果下方节点过多，剪枝底部节点
    if (container.children.length > this.viewerStream.maxDomLines + 200) {
      const excess = container.children.length - this.viewerStream.maxDomLines;
      for (let k = 0; k < excess; k++) {
        container.removeChild(container.lastChild);
      }
    }
  },

  // ================= 文件内搜索 (全文件后端检索与过滤模式) =================

  onViewerSearchInput() {
    clearTimeout(this.viewerSearchDebounceTimer);
    const input = document.getElementById("viewer-search-kw");
    const keyword = (input ? input.value : "").trim();
    const isCase = !!document.getElementById("viewer-search-case")?.checked;
    const isRegex = !!document.getElementById("viewer-search-regex")?.checked;
    const isWholeWord = !!document.getElementById("viewer-search-whole-word")?.checked;
    const isFilter = !!document.getElementById("viewer-search-filter-mode")?.checked;
    const counter = document.getElementById("viewer-search-counter");

    if (!keyword) {
      this.viewerFileSearchHits = [];
      this.viewerFileSearchIndex = -1;
      if (counter) counter.innerText = "0 / 0";
      this.clearViewerDOMHighlights();

      if (isFilter) {
        const viewer = document.getElementById("viewer-content");
        if (viewer) {
          viewer.innerHTML = `
            <div style="text-align: center; color: var(--text-dim); padding: 50px 20px; font-size: 13px;">
              🔍 过滤模式已开启，请在上方输入搜索关键词以筛选日志行
            </div>
          `;
        }
      } else if (this.viewerIsFilterMode) {
        this.viewerIsFilterMode = false;
        if (this.currentViewingFile) {
          this.loadFileContent(this.currentViewingFile.archiveID, this.currentViewingFile.relPath, this.currentViewingFile.size);
        }
      }
      return;
    }

    if (counter) {
      counter.innerHTML = `<span class="spinner-border spinner-border-sm" style="width: 10px; height: 10px; border-width: 1.5px;"></span> 全文搜...`;
    }

    this.viewerSearchDebounceTimer = setTimeout(() => {
      this.executeViewerFileSearch(keyword, isCase, isRegex, isWholeWord, isFilter);
    }, 300);
  },

  async executeViewerFileSearch(keyword, isCase, isRegex, isWholeWord, isFilter) {
    if (!this.currentViewingFile) return;
    const counter = document.getElementById("viewer-search-counter");

    if (this.viewerSearchAbortCtrl) {
      this.viewerSearchAbortCtrl.abort();
    }
    this.viewerSearchAbortCtrl = new AbortController();

    try {
      const payload = {
        archive_id: this.currentViewingFile.archiveID,
        file_path: this.currentViewingFile.relPath,
        keyword: keyword,
        is_regex: isRegex,
        case_sensitive: isCase,
        whole_word: isWholeWord,
        page: 1,
        page_size: 1000,
        context_lines: 0
      };

      const res = await this.api("/api/search", "POST", payload, { signal: this.viewerSearchAbortCtrl.signal });
      if (!res.ok) {
        if (counter) counter.innerText = "搜索失败";
        return;
      }
      const data = await res.json();
      this.viewerFileSearchHits = data.hits || [];
      this.viewerIsFilterMode = isFilter;

      if (this.viewerFileSearchHits.length === 0) {
        this.viewerFileSearchIndex = -1;
        if (counter) counter.innerText = "0 / 0";
        this.clearViewerDOMHighlights();

        if (isFilter) {
          const viewer = document.getElementById("viewer-content");
          if (viewer) {
            viewer.innerHTML = `
              <div style="text-align: center; color: var(--text-dim); padding: 50px 20px; font-size: 13px;">
                🔍 全文件未匹配到任何包含 “<strong>${this.escape(keyword)}</strong>” 的日志行
              </div>
            `;
          }
        }
        return;
      }

      // 确定首选激活的命中项
      let activeIndex = 0;
      if (this.viewerTargetLine > 0) {
        const foundIdx = this.viewerFileSearchHits.findIndex(h => h.line_number >= this.viewerTargetLine);
        if (foundIdx !== -1) {
          activeIndex = foundIdx;
        }
        this.viewerTargetLine = 0;
      } else if (!isFilter && this.viewerStream) {
        // 常规连续模式下，优先定位到当前视口可见区域或紧随其后的第一个匹配项
        const currentTopLine = this.viewerStream.baseStartLine + (this.viewerStream.renderedStartIdx || 0);
        const nextHitIdx = this.viewerFileSearchHits.findIndex(h => h.line_number >= currentTopLine);
        if (nextHitIdx !== -1) {
          activeIndex = nextHitIdx;
        }
      }
      this.viewerFileSearchIndex = activeIndex;

      const totalDisplay = (data.total > this.viewerFileSearchHits.length || this.viewerFileSearchHits.length >= 1000)
        ? `${this.viewerFileSearchHits.length}+`
        : `${this.viewerFileSearchHits.length}`;

      if (counter) {
        counter.innerText = `${activeIndex + 1} / ${totalDisplay}`;
      }

      if (isFilter) {
        // 过滤模式：仅显示包含关键字的日志
        this.renderViewerFilteredLines(keyword, isCase, isRegex, isWholeWord);
      } else {
        // 常规模式：高亮当前 DOM 视口中的匹配词，并平滑跳转至匹配行
        this.applyViewerSearchHighlights(keyword, isCase, isRegex, isWholeWord);
        const matchLine = this.viewerFileSearchHits[activeIndex].line_number;
        this.jumpToMatchLine(matchLine, keyword);
      }
    } catch (e) {
      if (e.name === 'AbortError') return;
      if (counter) counter.innerText = "搜索异常";
    }
  },

  onViewerFilterModeChange() {
    const isFilter = !!document.getElementById("viewer-search-filter-mode")?.checked;
    const input = document.getElementById("viewer-search-kw");
    const keyword = (input ? input.value : "").trim();
    const isCase = !!document.getElementById("viewer-search-case")?.checked;
    const isRegex = !!document.getElementById("viewer-search-regex")?.checked;
    const isWholeWord = !!document.getElementById("viewer-search-whole-word")?.checked;

    this.viewerIsFilterMode = isFilter;

    if (isFilter) {
      if (keyword) {
        if (this.viewerFileSearchHits && this.viewerFileSearchHits.length > 0) {
          this.renderViewerFilteredLines(keyword, isCase, isRegex, isWholeWord);
        } else {
          this.executeViewerFileSearch(keyword, isCase, isRegex, isWholeWord, true);
        }
      } else {
        const viewer = document.getElementById("viewer-content");
        if (viewer) {
          viewer.innerHTML = `
            <div style="text-align: center; color: var(--text-dim); padding: 50px 20px; font-size: 13px;">
              🔍 过滤模式已开启，请在上方输入搜索关键词以筛选日志行
            </div>
          `;
        }
      }
    } else {
      // 取消过滤模式：恢复常规连续滚动日志视图
      let jumpLine = 1;
      if (this.viewerFileSearchHits && this.viewerFileSearchIndex >= 0 && this.viewerFileSearchIndex < this.viewerFileSearchHits.length) {
        jumpLine = this.viewerFileSearchHits[this.viewerFileSearchIndex].line_number;
      }
      if (this.currentViewingFile) {
        this.loadFileContent(this.currentViewingFile.archiveID, this.currentViewingFile.relPath, this.currentViewingFile.size, jumpLine, keyword);
      }
    }
  },

  renderViewerFilteredLines(keyword, isCase, isRegex, isWholeWord) {
    const viewer = document.getElementById("viewer-content");
    if (!viewer) return;

    const hits = this.viewerFileSearchHits || [];
    if (hits.length === 0) {
      viewer.innerHTML = `
        <div style="text-align: center; color: var(--text-dim); padding: 50px 20px; font-size: 13px;">
          🔍 全文件未匹配到任何包含 “<strong>${this.escape(keyword)}</strong>” 的日志行
        </div>
      `;
      return;
    }

    let reg;
    try {
      let pattern = isRegex ? keyword : this.escapeRegex(keyword);
      if (isWholeWord) {
        pattern = `\\b(?:${pattern})\\b`;
      }
      reg = new RegExp(`(${pattern})`, isCase ? "g" : "gi");
    } catch (e) {
      reg = null;
    }

    const totalDisplay = (hits.length >= 1000) ? `${hits.length}+` : `${hits.length}`;
    let html = `
      <div class="v-filter-header">
        <span style="color: var(--primary); font-weight: 600;">
          ✨ 过滤模式生效中：全文件共匹配到 <strong>${totalDisplay}</strong> 行日志
        </span>
        <span style="color: var(--text-dim); font-size: 11px;">
          点击每行右侧「🔍 上下文」可返回连续视图查看该行前后日志
        </span>
      </div>
      <div id="v-filtered-lines-container" style="display: flex; flex-direction: column; width: 100%;">
    `;

    hits.forEach((h, idx) => {
      let replacedText = this.escape(h.content);
      if (reg) {
        reg.lastIndex = 0;
        replacedText = replacedText.replace(reg, '<mark class="v-match">$1</mark>');
      }
      const isCurrentActive = (idx === this.viewerFileSearchIndex);
      html += `
        <div class="v-line ${isCurrentActive ? 'v-line-highlight' : ''}" id="v-filter-line-${h.line_number}" data-line="${h.line_number}" onclick="app.selectFilteredLine(${idx})">
          <span class="v-line-no">${h.line_number}</span>
          <span class="v-line-text">${replacedText}</span>
          <span class="v-filter-ctx-btn" onclick="event.stopPropagation(); app.exitFilterAndJumpToLine(${h.line_number})" title="退出过滤模式并在连续日志中定位到此行">🔍 上下文</span>
        </div>
      `;
    });

    html += `</div>`;
    viewer.innerHTML = html;

    // 视口平滑定位到当前激活行
    if (this.viewerFileSearchIndex >= 0 && this.viewerFileSearchIndex < hits.length) {
      const activeLineNo = hits[this.viewerFileSearchIndex].line_number;
      const activeEl = document.getElementById(`v-filter-line-${activeLineNo}`);
      if (activeEl) {
        activeEl.scrollIntoView({ block: "center", behavior: "smooth" });
      }
    }
  },

  selectFilteredLine(idx) {
    if (!this.viewerFileSearchHits || idx < 0 || idx >= this.viewerFileSearchHits.length) return;
    this.viewerFileSearchIndex = idx;
    const counter = document.getElementById("viewer-search-counter");
    if (counter) {
      const isCapped = this.viewerFileSearchHits.length >= 1000;
      counter.innerText = `${idx + 1} / ${this.viewerFileSearchHits.length}${isCapped ? '+' : ''}`;
    }

    document.querySelectorAll("#viewer-content .v-line.v-line-highlight").forEach(el => el.classList.remove("v-line-highlight"));
    const lineNo = this.viewerFileSearchHits[idx].line_number;
    const targetEl = document.getElementById(`v-filter-line-${lineNo}`);
    if (targetEl) {
      targetEl.classList.add("v-line-highlight");
    }
  },

  exitFilterAndJumpToLine(lineNo) {
    const filterCheckbox = document.getElementById("viewer-search-filter-mode");
    if (filterCheckbox) filterCheckbox.checked = false;
    this.viewerIsFilterMode = false;

    const input = document.getElementById("viewer-search-kw");
    const keyword = (input ? input.value : "").trim();

    if (this.currentViewingFile) {
      this.loadFileContent(this.currentViewingFile.archiveID, this.currentViewingFile.relPath, this.currentViewingFile.size, lineNo, keyword);
    }
  },

  viewerSearchNext() {
    if (!this.viewerFileSearchHits || this.viewerFileSearchHits.length === 0) return;
    this.viewerFileSearchIndex = (this.viewerFileSearchIndex + 1) % this.viewerFileSearchHits.length;
    this.focusCurrentSearchHit();
  },

  viewerSearchPrev() {
    if (!this.viewerFileSearchHits || this.viewerFileSearchHits.length === 0) return;
    this.viewerFileSearchIndex = (this.viewerFileSearchIndex - 1 + this.viewerFileSearchHits.length) % this.viewerFileSearchHits.length;
    this.focusCurrentSearchHit();
  },

  focusCurrentSearchHit() {
    const hits = this.viewerFileSearchHits;
    const idx = this.viewerFileSearchIndex;
    if (!hits || idx < 0 || idx >= hits.length) return;

    const counter = document.getElementById("viewer-search-counter");
    if (counter) {
      const isCapped = hits.length >= 1000;
      counter.innerText = `${idx + 1} / ${hits.length}${isCapped ? '+' : ''}`;
    }

    const targetLine = hits[idx].line_number;
    const input = document.getElementById("viewer-search-kw");
    const keyword = (input ? input.value : "").trim();

    if (this.viewerIsFilterMode) {
      document.querySelectorAll("#viewer-content .v-line.v-line-highlight").forEach(el => el.classList.remove("v-line-highlight"));
      const targetEl = document.getElementById(`v-filter-line-${targetLine}`);
      if (targetEl) {
        targetEl.classList.add("v-line-highlight");
        targetEl.scrollIntoView({ block: "center", behavior: "smooth" });
      }
    } else {
      this.jumpToMatchLine(targetLine, keyword);
    }
  },

  jumpToMatchLine(targetLine, keyword) {
    const targetEl = document.getElementById(`v-line-${targetLine}`);
    if (targetEl) {
      document.querySelectorAll("#v-lines-container .v-line.v-line-highlight").forEach(el => el.classList.remove("v-line-highlight"));
      targetEl.classList.add("v-line-highlight");
      targetEl.scrollIntoView({ block: "center", behavior: "smooth" });

      // 激活其内部匹配标记
      document.querySelectorAll("#v-lines-container mark.v-match.v-match-active").forEach(m => m.classList.remove("v-match-active"));
      const markEl = targetEl.querySelector("mark.v-match");
      if (markEl) {
        markEl.classList.add("v-match-active");
      }
    } else if (this.currentViewingFile) {
      // 目标行未在当前视口中（如在第几万行），从服务端载入以该行为中心的新窗口
      this.loadFileContent(this.currentViewingFile.archiveID, this.currentViewingFile.relPath, this.currentViewingFile.size, targetLine, keyword);
    }
  },

  clearViewerDOMHighlights() {
    const lines = document.querySelectorAll("#v-lines-container .v-line");
    lines.forEach((lineEl) => {
      lineEl.classList.remove("v-line-highlight");
      const lineNo = parseInt(lineEl.dataset.line, 10);
      const textSpan = lineEl.querySelector(".v-line-text");
      if (textSpan && this.viewerStream) {
        const rawIdx = lineNo - this.viewerStream.baseStartLine;
        if (rawIdx >= 0 && this.viewerStream.loadedLines[rawIdx] !== undefined) {
          textSpan.innerText = this.viewerStream.loadedLines[rawIdx];
        }
      }
    });
  },

  applyViewerSearchHighlights(keyword, isCase, isRegex, isWholeWord) {
    this.clearViewerDOMHighlights();
    if (!keyword) return;

    let reg;
    try {
      let pattern = isRegex ? keyword : this.escapeRegex(keyword);
      if (isWholeWord) {
        pattern = `\\b(?:${pattern})\\b`;
      }
      reg = new RegExp(`(${pattern})`, isCase ? "g" : "gi");
    } catch (e) {
      return;
    }

    const lines = document.querySelectorAll("#v-lines-container .v-line");
    lines.forEach((lineEl) => {
      const lineNo = parseInt(lineEl.dataset.line, 10);
      const rawIdx = lineNo - this.viewerStream.baseStartLine;
      const raw = this.viewerStream ? this.viewerStream.loadedLines[rawIdx] : null;
      if (!raw || !reg.test(raw)) return;
      reg.lastIndex = 0;

      const textSpan = lineEl.querySelector(".v-line-text");
      if (!textSpan) return;

      const replaced = this.escape(raw).replace(reg, (match) => {
        return `<mark class="v-match">${match}</mark>`;
      });
      textSpan.innerHTML = replaced;
    });
  },

  onViewerSearchKeydown(event) {
    if (event.key === "Enter") {
      event.preventDefault();
      if (event.shiftKey) {
        this.viewerSearchPrev();
      } else {
        this.viewerSearchNext();
      }
    }
  },

  jumpToLineNo() {
    const input = document.getElementById("viewer-jump-lineno");
    const lineNo = parseInt(input ? input.value : "", 10);
    if (!lineNo || lineNo < 1) return;

    const targetEl = document.getElementById(`v-line-${lineNo}`);
    if (targetEl) {
      document.querySelectorAll(".v-line.v-line-highlight").forEach(el => el.classList.remove("v-line-highlight"));
      targetEl.classList.add("v-line-highlight");
      targetEl.scrollIntoView({ block: "center", behavior: "smooth" });
    } else if (this.currentViewingFile) {
      // 若当前行未在已渲染视口中，从服务端加载以该行为中心的新窗口
      this.loadFileContent(this.currentViewingFile.archiveID, this.currentViewingFile.relPath, 0, lineNo, "");
    }
  },

  // 跨页面或从搜索结果中打开日志查看器并直达指定行
  async openViewerAndJump(archiveID, relPath, lineNo, keyword = "") {
    await this.viewFiles(archiveID);
    this.ensureParentDirsExpanded(relPath);
    this.renderFileTree(archiveID, this.browserFiles, this.currentTreeFilter);
    this.loadFileContent(archiveID, relPath, 0, lineNo, keyword);
  },

  // 下载指定归档包内的具体日志文件
  downloadFile(archiveID, relPath) {
    if (!archiveID || !relPath) return;
    const tokenParam = this.token ? `&token=${encodeURIComponent(this.token)}` : '';
    const url = `/api/archives/${archiveID}/download-file?path=${encodeURIComponent(relPath)}${tokenParam}`;
    const a = document.createElement("a");
    a.href = url;
    a.download = relPath.split("/").pop() || "log_file";
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  },

  // 下载原始日志归档压缩包
  downloadArchive(archiveID) {
    if (!archiveID) return;
    const tokenParam = this.token ? `?token=${encodeURIComponent(this.token)}` : '';
    const url = `/api/archives/${archiveID}/download${tokenParam}`;
    const a = document.createElement("a");
    a.href = url;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  },

  downloadCurrentViewingFile() {
    if (this.currentViewingFile) {
      this.downloadFile(this.currentViewingFile.archiveID, this.currentViewingFile.relPath);
    }
  },

  downloadCurrentArchive() {
    if (this.currentViewingArchiveID) {
      this.downloadArchive(this.currentViewingArchiveID);
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

      <h4 style="font-size: 14px; margin-bottom: 14px;">🔎 命中分布式系统故障特征事件 (共 ${rep.total_events} 处):</h4>
      <div>${eventsHtml}</div>

      <hr style="border: none; border-top: 1px solid var(--border-color); margin: 20px 0;">
      <div id="diag-templates-section">
        <h4 style="font-size: 14px; margin-bottom: 10px; color: #38bdf8;">🧩 通用日志模式模板挖掘与聚类降噪 (Drain Top Patterns):</h4>
        <div id="diag-templates-content" style="color: var(--text-dim); font-size: 12px;">正在挖掘分析高频模式...</div>
      </div>
    `;

    this.loadArchiveTemplates(rep.archive_id);
  },

  async loadArchiveTemplates(archiveID) {
    const box = document.getElementById("diag-templates-content");
    if (!box) return;
    try {
      const res = await this.api(`/api/analysis/templates?archive_id=${archiveID}`);
      if (!res.ok) {
        box.innerHTML = `<span style="color: var(--text-dim);">模板挖掘暂不可用或无数据。</span>`;
        return;
      }
      const templates = await res.json();
      if (!templates || templates.length === 0) {
        box.innerHTML = `<span style="color: var(--text-dim);">未提取到聚合模板。</span>`;
        return;
      }

      let html = `
        <div class="table-responsive">
          <table class="table" style="font-size: 12px;">
            <thead>
              <tr>
                <th style="width: 80px;">级别</th>
                <th style="width: 70px;">频次</th>
                <th>聚类模式 (Pattern)</th>
                <th>原始采样日志 (Sample)</th>
              </tr>
            </thead>
            <tbody>
      `;
      templates.slice(0, 10).forEach(t => {
        const lvlClass = (t.level || "INFO").toLowerCase();
        const targetArchID = t.archive_id || archiveID;
        const sampleFile = t.sample_file || "";
        const sampleLine = t.sample_line || 1;
        const hasJumpTarget = !!(targetArchID && sampleFile);

        let sampleTd = "";
        if (hasJumpTarget) {
          sampleTd = `
            <td style="font-family: monospace; font-size: 11px; cursor: pointer; max-width: 380px;"
                onclick="app.openViewerAndJump('${this.escape(targetArchID)}', '${this.escape(sampleFile)}', ${sampleLine})"
                title="点击在解包查看器中直达 ${this.escape(sampleFile)} 第 ${sampleLine} 行">
              <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 2px;">
                <span style="color: #38bdf8; font-size: 10px;">📄 ${this.escape(sampleFile)} : L${sampleLine}</span>
                <span class="badge badge-primary" style="font-size: 9px; padding: 1px 4px;">👁️ 查看</span>
              </div>
              <div style="color: var(--text-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">
                ${this.escape(t.sample)}
              </div>
            </td>
          `;
        } else {
          sampleTd = `
            <td style="color: var(--text-dim); font-family: monospace; font-size: 11px; max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;" title="${this.escape(t.sample)}">
              ${this.escape(t.sample)}
            </td>
          `;
        }

        html += `
          <tr>
            <td><span class="badge badge-${lvlClass === 'error' || lvlClass === 'fatal' ? 'danger' : lvlClass === 'warn' ? 'warning' : 'muted'}">${t.level || 'INFO'}</span></td>
            <td><strong>${t.count}</strong> 次</td>
            <td><code style="color: #38bdf8; font-size: 11px;">${this.escape(t.pattern)}</code></td>
            ${sampleTd}
          </tr>
        `;
      });
      html += `</tbody></table></div>`;
      box.innerHTML = html;
    } catch (e) {
      box.innerHTML = `<span style="color: var(--danger);">加载模板失败: ${e.message}</span>`;
    }
  },

  // ================= 基准差分对比 (Baseline Diff) =================

  populateDiffArchiveSelects() {
    const selA = document.getElementById("diff-archive-a");
    const selB = document.getElementById("diff-archive-b");
    const selParent = document.getElementById("diff-parent-archive");

    let opts = '<option value="">-- 请选择日志包 --</option>';
    let readyArchives = [];
    this.archives.forEach(a => {
      if (a.status === "ready") {
        readyArchives.push(a);
        const tagText = a.tags && a.tags.length > 0 ? ` [${a.tags.join(", ")}]` : "";
        const sizeStr = a.size > 1024 * 1024 ? `${(a.size / (1024 * 1024)).toFixed(1)} MB` : `${(a.size / 1024).toFixed(1)} KB`;
        const fullTitle = `${a.filename}${tagText} (${sizeStr}, ${a.file_count || 0} 个文件)`;
        opts += `<option value="${a.id}" title="${this.escape(fullTitle)}">${this.escape(a.filename)}${this.escape(tagText)} (${sizeStr})</option>`;
      }
    });

    if (selA) selA.innerHTML = opts;
    if (selB) selB.innerHTML = opts;
    if (selParent) selParent.innerHTML = opts;

    if (readyArchives.length >= 2) {
      if (selA) selA.selectedIndex = 1;
      if (selB) selB.selectedIndex = 2;
    }
    if (readyArchives.length >= 1 && selParent) {
      selParent.selectedIndex = 1;
    }

    this.updateDiffArchivePreview('a');
    this.updateDiffArchivePreview('b');
    this.updateDiffParentPreview();
  },

  updateDiffArchivePreview(side) {
    const sel = document.getElementById(`diff-archive-${side}`);
    const previewEl = document.getElementById(`diff-preview-${side}`);
    const tagEl = document.getElementById(`diff-tag-${side}`);
    if (!sel || !previewEl) return;

    const archiveID = sel.value;
    if (!archiveID) {
      previewEl.style.display = "none";
      if (tagEl) tagEl.innerText = "";
      sel.title = "";
      return;
    }

    const a = this.archives.find(item => item.id === archiveID);
    if (!a) {
      previewEl.style.display = "none";
      return;
    }

    sel.title = `${a.filename} (${(a.size / (1024 * 1024)).toFixed(1)} MB)`;
    const tags = a.tags && a.tags.length > 0 ? a.tags.join(", ") : "无";
    if (tagEl) tagEl.innerText = a.tags && a.tags.length > 0 ? `标签: ${tags}` : "";

    const sizeStr = a.size > 1024 * 1024 ? `${(a.size / (1024 * 1024)).toFixed(2)} MB` : `${(a.size / 1024).toFixed(1)} KB`;
    const uploadTime = a.upload_time ? new Date(a.upload_time).toLocaleString() : "-";
    const nodeName = a.storage_node_name || a.assigned_worker || "本地主节点";

    previewEl.innerHTML = `
      <div class="filename-full" title="${this.escape(a.filename)}">📄 ${this.escape(a.filename)}</div>
      <div class="meta-line">
        <span class="meta-item">💾 大小: <strong>${sizeStr}</strong></span>
        <span class="meta-item">📁 文件数: <strong>${a.file_count || 0}</strong></span>
        <span class="meta-item">📝 总行数: <strong>${(a.total_lines || 0).toLocaleString()}</strong></span>
        <span class="meta-item">🖥️ 节点: <strong>${this.escape(nodeName)}</strong></span>
        <span class="meta-item">🏷️ 标签: <strong>${this.escape(tags)}</strong></span>
        <span class="meta-item">🕒 上传: <strong>${uploadTime}</strong></span>
      </div>
    `;
    previewEl.style.display = "flex";
  },

  updateDiffParentPreview() {
    const sel = document.getElementById("diff-parent-archive");
    const previewEl = document.getElementById("diff-parent-preview");
    if (!sel || !previewEl) return;
    const archiveID = sel.value;
    if (!archiveID) {
      previewEl.style.display = "none";
      return;
    }
    const a = this.archives.find(item => item.id === archiveID);
    if (!a) {
      previewEl.style.display = "none";
      return;
    }
    sel.title = a.filename;
    const sizeStr = a.size > 1024 * 1024 ? `${(a.size / (1024 * 1024)).toFixed(2)} MB` : `${(a.size / 1024).toFixed(1)} KB`;
    previewEl.innerHTML = `
      <div class="filename-full">📦 ${this.escape(a.filename)}</div>
      <div class="meta-line">
        <span class="meta-item">💾 压缩包大小: <strong>${sizeStr}</strong></span>
        <span class="meta-item">📁 解压文件总数: <strong>${a.file_count || 0}</strong></span>
        <span class="meta-item">🖥️ 存储节点: <strong>${this.escape(a.storage_node_name || "本地主节点")}</strong></span>
      </div>
    `;
    previewEl.style.display = "flex";
  },

  updateDiffSubArchivePreview(side) {
    const sel = document.getElementById(`diff-sub-archive-${side}`);
    const previewEl = document.getElementById(`diff-sub-preview-${side}`);
    if (!sel || !previewEl) return;
    const subPath = sel.value;
    if (!subPath) {
      previewEl.style.display = "none";
      return;
    }
    const selectedOpt = sel.options[sel.selectedIndex];
    const text = selectedOpt ? selectedOpt.text : subPath;
    sel.title = text;
    previewEl.innerHTML = `
      <div class="filename-full">🧩 ${this.escape(subPath)}</div>
      <div class="meta-line">
        <span class="meta-item">${this.escape(text)}</span>
      </div>
    `;
    previewEl.style.display = "flex";
  },

  onDiffModeChange() {
    const isIntra = document.querySelector('input[name="diff-mode"]:checked')?.value === "intra";
    const secInter = document.getElementById("diff-selector-inter");
    const secIntra = document.getElementById("diff-selector-intra");

    if (isIntra) {
      if (secInter) secInter.style.display = "none";
      if (secIntra) secIntra.style.display = "block";
      this.onDiffParentArchiveChange();
    } else {
      if (secInter) secInter.style.display = "block";
      if (secIntra) secIntra.style.display = "none";
      this.updateDiffArchivePreview('a');
      this.updateDiffArchivePreview('b');
    }
  },

  async onDiffParentArchiveChange() {
    const selParent = document.getElementById("diff-parent-archive");
    const archiveID = selParent?.value;
    const selSubA = document.getElementById("diff-sub-archive-a");
    const selSubB = document.getElementById("diff-sub-archive-b");
    const hint = document.getElementById("diff-sub-hint");

    this.updateDiffParentPreview();

    if (!archiveID) {
      if (selSubA) selSubA.innerHTML = '<option value="">-- 请先选择外层日志包 --</option>';
      if (selSubB) selSubB.innerHTML = '<option value="">-- 请先选择外层日志包 --</option>';
      this.updateDiffSubArchivePreview('a');
      this.updateDiffSubArchivePreview('b');
      if (hint) hint.innerHTML = "";
      return;
    }

    if (selSubA) selSubA.innerHTML = '<option value="">正在探测包内子包与节点...</option>';
    if (selSubB) selSubB.innerHTML = '<option value="">正在探测包内子包与节点...</option>';
    if (hint) hint.innerHTML = "⏳ 正在扫描该压缩包内所有解压出来的独立子压缩包、多层嵌套包与子模块目录...";

    try {
      const res = await this.api(`/api/archives/${archiveID}/sub-archives`);
      if (!res.ok) throw new Error(await res.text());
      const subs = await res.json();

      if (!Array.isArray(subs) || subs.length === 0) {
        const emptyOpt = '<option value="">未检测到独立子包目录</option>';
        if (selSubA) selSubA.innerHTML = emptyOpt;
        if (selSubB) selSubB.innerHTML = emptyOpt;
        this.updateDiffSubArchivePreview('a');
        this.updateDiffSubArchivePreview('b');
        if (hint) {
          hint.innerHTML = '<span style="color: var(--warning);">⚠️ 该日志包内所有文件均直接平铺在根目录下，未检测到多个独立子包/节点模块。建议使用上方“跨日志包对比”模式。</span>';
        }
        return;
      }

      let subOpts = '<option value="">-- 请选择参与对比的内部子包/模块 --</option>';
      subs.forEach(s => {
        const sizeStr = s.total_size > 1024 * 1024 ? `${(s.total_size / (1024 * 1024)).toFixed(1)} MB` : `${(s.total_size / 1024).toFixed(1)} KB`;
        const nestedBadge = s.has_nested ? " [含深层嵌套]" : "";
        subOpts += `<option value="${this.escape(s.path)}" title="${this.escape(s.path)} (${s.total_files} 文件, ${sizeStr})">📦 ${this.escape(s.name)}${nestedBadge} (${s.total_files} 个文件, ${sizeStr})</option>`;
      });

      if (selSubA) selSubA.innerHTML = subOpts;
      if (selSubB) selSubB.innerHTML = subOpts;

      if (subs.length >= 2) {
        if (selSubA) selSubA.selectedIndex = 1;
        if (selSubB) selSubB.selectedIndex = 2;
        if (hint) {
          hint.innerHTML = `<span style="color: #34d399;">✔ 智能识别出该包内包含 <strong>${subs.length}</strong> 个内部独立子压缩包/节点，已自动为您预选前两项进行控制变量差分诊断。</span>`;
        }
      } else if (subs.length === 1) {
        if (selSubA) selSubA.selectedIndex = 1;
        if (hint) {
          hint.innerHTML = `<span style="color: #fbbf24;">ℹ️ 该包仅探测到 1 个子模块 [${subs[0].name}]，同包差分至少需要 2 个不同子包参与对比。</span>`;
        }
      }
      this.updateDiffSubArchivePreview('a');
      this.updateDiffSubArchivePreview('b');
    } catch (err) {
      if (hint) hint.innerHTML = `<span style="color: var(--danger);">探测包内子包失败: ${err.message}</span>`;
    }
  },

  async executeDiffAnalysis() {
    const isIntra = document.querySelector('input[name="diff-mode"]:checked')?.value === "intra";
    let idA = "";
    let idB = "";
    let subA = "";
    let subB = "";

    const btnInter = document.getElementById("btn-run-diff-inter");
    const btnIntra = document.getElementById("btn-run-diff-intra");
    const activeBtn = isIntra ? btnIntra : btnInter;

    if (isIntra) {
      const parentID = document.getElementById("diff-parent-archive")?.value;
      subA = document.getElementById("diff-sub-archive-a")?.value;
      subB = document.getElementById("diff-sub-archive-b")?.value;

      if (!parentID) {
        alert("请先选择目标综合聚合日志包");
        return;
      }
      if (!subA || !subB) {
        alert("请同时选择包内的基准子包 A 与待测子包 B");
        return;
      }
      if (subA === subB) {
        alert("同一包内对比必须选择两个不同的内部子包或节点！");
        return;
      }
      idA = parentID;
      idB = parentID;
    } else {
      idA = document.getElementById("diff-archive-a")?.value;
      idB = document.getElementById("diff-archive-b")?.value;
      if (!idA || !idB) {
        alert("请同时选择基准日志包 A 与待测日志包 B");
        return;
      }
      if (idA === idB) {
        alert("跨包对比请选择两份不同的归档包！若对比同一个压缩包内的不同子包，请切换至【同一压缩包内多子包对比】模式。");
        return;
      }
    }

    if (activeBtn) {
      activeBtn.disabled = true;
      activeBtn.innerText = "正在深度差分计算...";
    }

    const resCard = document.getElementById("diff-result-card");
    try {
      const res = await this.api("/api/analysis/diff", "POST", {
        archive_id_a: idA,
        sub_path_a: subA,
        archive_id_b: idB,
        sub_path_b: subB,
      });
      if (!res.ok) {
        throw new Error(await res.text());
      }
      const data = await res.json();
      resCard.style.display = "block";
      document.getElementById("diff-analyzed-at").innerText = `对比时间: ${new Date(data.analyzed_at).toLocaleTimeString()}`;
      document.getElementById("diff-stat-files").innerText = `${data.total_files_a} vs ${data.total_files_b}`;
      document.getElementById("diff-stat-events").innerText = `${data.total_events_a} vs ${data.total_events_b}`;
      document.getElementById("diff-stat-new-patterns").innerText = (data.new_templates ? data.new_templates.length : 0);

      document.getElementById("diff-summary-alert").innerText = data.summary_text;

      // 渲染独有新增异常模式
      const tbody = document.getElementById("diff-new-templates-tbody");
      if (data.new_templates && data.new_templates.length > 0) {
        tbody.innerHTML = data.new_templates.map(t => {
          const lvl = (t.level || "INFO").toLowerCase();
          const targetArchID = t.archive_id || data.archive_id_b;
          const sampleFile = t.sample_file || "";
          const sampleLine = t.sample_line || 1;
          const hasJumpTarget = !!(targetArchID && sampleFile);

          let sampleHtml = "";
          if (hasJumpTarget) {
            sampleHtml = `
              <div style="cursor: pointer; padding: 6px 10px; border-radius: 6px; background: rgba(15, 23, 42, 0.5); border: 1px solid rgba(56, 189, 248, 0.25); transition: all 0.2s;"
                   onmouseover="this.style.background='rgba(56, 189, 248, 0.12)'; this.style.borderColor='rgba(56, 189, 248, 0.6)';"
                   onmouseout="this.style.background='rgba(15, 23, 42, 0.5)'; this.style.borderColor='rgba(56, 189, 248, 0.25)';"
                   onclick="app.openViewerAndJump('${this.escape(targetArchID)}', '${this.escape(sampleFile)}', ${sampleLine})"
                   title="点击在解包查看器中直达 ${this.escape(sampleFile)} 第 ${sampleLine} 行">
                <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px;">
                  <span style="color: #38bdf8; font-size: 11px; font-weight: 600;">
                    📄 ${this.escape(sampleFile)} <span style="color: #fbbf24; font-weight: normal;">(第 ${sampleLine} 行)</span>
                  </span>
                  <span class="badge badge-primary" style="font-size: 10px; padding: 2px 6px; line-height: 1.2;">👁️ 定位直达</span>
                </div>
                <div style="color: var(--text-color, #e2e8f0); font-family: monospace; font-size: 11px; white-space: pre-wrap; word-break: break-all; line-height: 1.4;">
                  ${this.escape(t.sample)}
                </div>
              </div>
            `;
          } else {
            sampleHtml = `<div style="color: var(--text-dim); font-family: monospace; font-size: 11px; white-space: pre-wrap; word-break: break-all;">${this.escape(t.sample)}</div>`;
          }

          return `
            <tr>
              <td><span class="badge badge-${lvl === 'error' || lvl === 'fatal' ? 'danger' : lvl === 'warn' ? 'warning' : 'muted'}">${t.level || 'INFO'}</span></td>
              <td><strong>${t.count}</strong> 次</td>
              <td><code style="color: #fbbf24; font-size: 11px; word-break: break-all;">${this.escape(t.pattern)}</code></td>
              <td>${sampleHtml}</td>
            </tr>
          `;
        }).join("");
      } else {
        tbody.innerHTML = `<tr><td colspan="4" style="text-align: center; color: var(--success); padding: 15px;">✔ 待测目标未见突发异质日志模式！</td></tr>`;
      }

      // 渲染文件增减清单
      const addedList = document.getElementById("diff-added-files-list");
      const removedList = document.getElementById("diff-removed-files-list");
      document.getElementById("diff-added-files-count").innerText = data.added_files ? data.added_files.length : 0;
      document.getElementById("diff-removed-files-count").innerText = data.removed_files ? data.removed_files.length : 0;

      addedList.innerHTML = (data.added_files && data.added_files.length > 0)
        ? data.added_files.map(f => `<div>+ ${this.escape(f)}</div>`).join("")
        : '<div style="color: var(--text-dim);">无新增文件</div>';

      removedList.innerHTML = (data.removed_files && data.removed_files.length > 0)
        ? data.removed_files.map(f => `<div>- ${this.escape(f)}</div>`).join("")
        : '<div style="color: var(--text-dim);">无缺失文件</div>';

      resCard.scrollIntoView({ behavior: "smooth" });
    } catch (err) {
      alert("差分比对失败: " + err.message);
    } finally {
      if (activeBtn) {
        activeBtn.disabled = false;
        activeBtn.innerText = isIntra ? "🚀 执行包内深度差分比对" : "🚀 执行跨包差分比对";
      }
    }
  },

  // ================= 日志全局检索 =================

  populateSearchArchiveSelect() {
    const sel = document.getElementById("search-archive-select");
    sel.innerHTML = '<option value="">-- 请选择日志包 --</option>';
    this.archives.forEach(a => {
      if (a.status === "ready") {
        const tagText = a.tags && a.tags.length > 0 ? ` [${a.tags.join(", ")}]` : "";
        sel.innerHTML += `<option value="${a.id}">${this.escape(a.filename)}${this.escape(tagText)} (${(a.size / (1024 * 1024)).toFixed(1)} MB)</option>`;
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
    const wholeWord = !!document.getElementById("search-whole-word")?.checked;
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
        whole_word: wholeWord,
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

      this.renderSearchHistogram(data.histogram, data.extracted_traces);
      this.renderSearchHits(data.hits, keyword, isRegex, caseSensitive, wholeWord);
    } catch (e) {
      alert("检索失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.innerText = "🔎 执行检索";
    }
  },

  renderSearchHistogram(histogram, traces) {
    const histCard = document.getElementById("search-histogram-card");
    const chart = document.getElementById("search-histogram-chart");
    const traceChips = document.getElementById("search-trace-chips");
    const traceList = document.getElementById("search-trace-list");
    if (!histCard || !chart) return;

    if (!histogram || histogram.length === 0) {
      histCard.style.display = "none";
      return;
    }

    histCard.style.display = "block";
    let maxCount = 1;
    histogram.forEach(b => {
      if (b.total_count > maxCount) maxCount = b.total_count;
    });

    let barsHtml = `<div style="display: flex; align-items: flex-end; width: 100%; height: 80px; gap: 3px; padding-bottom: 20px; box-sizing: border-box; position: relative;">`;

    histogram.forEach((b, i) => {
      const heightPercent = Math.max(8, Math.round((b.total_count / maxCount) * 100));
      const timeShort = b.timestamp.length >= 16 ? b.timestamp.substring(11, 16) : b.timestamp;
      barsHtml += `
        <div style="flex: 1; height: 100%; display: flex; flex-direction: column; justify-content: flex-end; position: relative; cursor: pointer;" 
             title="时间: ${b.timestamp}\n总计: ${b.total_count} 条\nFATAL: ${b.fatal_count} | ERROR: ${b.error_count} | WARN: ${b.warn_count} | INFO: ${b.info_count}\n点击快速聚焦" 
             onclick="app.zoomHistogram('${b.timestamp}')">
          <div style="width: 100%; height: ${heightPercent}%; display: flex; flex-direction: column; border-radius: 2px; overflow: hidden; background: #334155;">
            ${b.fatal_count > 0 ? `<div style="height: ${(b.fatal_count / b.total_count)*100}%; background: #ef4444;"></div>` : ''}
            ${b.error_count > 0 ? `<div style="height: ${(b.error_count / b.total_count)*100}%; background: #f59e0b;"></div>` : ''}
            ${b.warn_count > 0 ? `<div style="height: ${(b.warn_count / b.total_count)*100}%; background: #fbbf24;"></div>` : ''}
            ${b.info_count > 0 ? `<div style="height: ${(b.info_count / b.total_count)*100}%; background: #38bdf8;"></div>` : ''}
          </div>
          ${i % Math.ceil(histogram.length / 6) === 0 ? `<span style="position: absolute; bottom: 0; left: 0; font-size: 10px; color: var(--text-dim); white-space: nowrap;">${timeShort}</span>` : ''}
        </div>
      `;
    });
    barsHtml += `</div>`;
    chart.innerHTML = barsHtml;

    // 渲染识别出的 TraceID
    if (traces && traces.length > 0) {
      traceChips.style.display = "block";
      traceList.innerHTML = traces.map(tr => `
        <button type="button" class="btn btn-secondary btn-xs" style="font-family: monospace; font-size: 11px; padding: 2px 8px; border-radius: 12px; background: rgba(56, 189, 248, 0.15); border-color: #38bdf8; color: #38bdf8;" onclick="app.drillDownTrace('${this.escape(tr)}')">
          🔗 ${this.escape(tr)}
        </button>
      `).join("");
    } else {
      traceChips.style.display = "none";
    }
  },

  drillDownTrace(traceID) {
    const kwInput = document.getElementById("search-keyword");
    if (kwInput) {
      kwInput.value = traceID;
      document.getElementById("search-is-regex").checked = false;
      document.getElementById("search-whole-word").checked = false;
      this.doSearch(1);
    }
  },

  zoomHistogram(timestamp) {
    if (!timestamp) return;
    alert(`已聚焦所选故障波峰时间点: ${timestamp}\n可在上方关键词中补充该时刻或缩小检索范围。`);
  },

  renderSearchHits(hits, keyword, isRegex = false, caseSensitive = false, wholeWord = false) {
    const container = document.getElementById("search-hits-container");
    container.innerHTML = "";

    if (!hits || hits.length === 0) {
      container.innerHTML = `<p style="text-align: center; color: var(--text-dim); padding: 30px;">未匹配到符合条件的日志行</p>`;
      return;
    }

    hits.forEach(h => {
      const div = document.createElement("div");
      div.className = "search-hit-item";

      // 自动提取 TraceID
      const traceMatch = h.content.match(/(?:trace_?id|request_?id|req_?id)[=:]\s*([a-zA-Z0-9_-]+)/i);
      const traceID = traceMatch ? traceMatch[1] : "";

      let highlighted = this.escape(h.content);
      if (keyword) {
        let pattern = isRegex ? keyword : this.escapeRegex(keyword);
        if (wholeWord) {
          pattern = `\\b(?:${pattern})\\b`;
        }
        try {
          const reg = new RegExp(`(${pattern})`, caseSensitive ? "g" : "gi");
          highlighted = highlighted.replace(reg, '<span class="highlight">$1</span>');
        } catch (e) {}
      }

      let ctxHtml = "";
      if (h.context_before && h.context_before.length > 0) {
        ctxHtml += `<div class="hit-context">${h.context_before.map(l => this.escape(l)).join("<br>")}</div>`;
      }
      ctxHtml += `<div class="hit-line-content">${highlighted}</div>`;
      if (h.context_after && h.context_after.length > 0) {
        ctxHtml += `<div class="hit-context">${h.context_after.map(l => this.escape(l)).join("<br>")}</div>`;
      }

      const searchArchID = h.archive_id || document.getElementById("search-archive-select")?.value || "";
      div.innerHTML = `
        <div class="hit-header">
          <span><code>${this.escape(h.file_path)} : 第 ${h.line_number} 行</code></span>
          <div style="display: flex; align-items: center; gap: 6px;">
            ${traceID ? `
              <button class="btn btn-xs" style="background: rgba(56, 189, 248, 0.15); color: #38bdf8; border: 1px solid #38bdf8;" title="点击穿透下钻此 TraceID 调用链" onclick="app.drillDownTrace('${this.escape(traceID)}')">🔗 Trace: ${this.escape(traceID)}</button>
            ` : ''}
            ${searchArchID ? `
              <button class="btn btn-primary btn-xs" title="在解包查看器中定位此行并阅读上下文" onclick="app.openViewerAndJump('${searchArchID}', '${this.escape(h.file_path)}', ${h.line_number}, '${this.escape(keyword || '')}')">👁️ 定位查看</button>
              <button class="btn btn-secondary btn-xs" title="下载此日志文件" onclick="app.downloadFile('${searchArchID}', '${this.escape(h.file_path)}')">📥 下载日志</button>
            ` : ''}
            <span class="badge ${h.level === "ERROR" || h.level === "FATAL" ? "badge-danger" : h.level === "WARN" ? "badge-warning" : "badge-muted"}">${h.level}</span>
            <span style="color: var(--text-dim); font-size: 11px; margin-left: 4px;">${h.timestamp || ""}</span>
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
      const pathScope = r.file_path_pattern 
        ? `<code style="font-size: 11px; color: #38bdf8; background: rgba(56, 189, 248, 0.1); padding: 2px 6px; border-radius: 4px;">${this.escape(r.file_path_pattern)}</code>`
        : `<span class="badge badge-muted">全部日志</span>`;

      tr.innerHTML = `
        <td><strong>${this.escape(r.name)}</strong></td>
        <td><span class="badge badge-info">${r.storage_type}</span></td>
        <td><span class="badge badge-${sevClass}">${r.severity}</span></td>
        <td><code style="font-size: 11px;">${this.escape(r.pattern)}</code></td>
        <td>${pathScope}</td>
        <td><span class="badge badge-muted">${r.is_regex ? "正则" : "关键词"}</span></td>
        <td style="max-width: 300px; font-size: 12px; color: var(--text-muted);">${this.escape(r.suggestion)}</td>
        <td><span class="badge ${r.enabled ? "badge-success" : "badge-muted"}">${r.enabled ? "已启用" : "已禁用"}</span></td>
        <td>
          ${this.currentUser?.role === "admin" ? `
            <div style="display: flex; gap: 6px;">
              <button class="btn btn-secondary btn-sm" onclick="app.openEditRuleModal('${r.id}')">编辑</button>
              <button class="btn btn-danger btn-sm" onclick="app.deleteRule('${r.id}')">删除</button>
            </div>
          ` : '-'}
        </td>
      `;
      tbody.appendChild(tr);
    });
  },

  openCreateRuleModal() {
    document.getElementById("rule-id").value = "";
    document.getElementById("rule-name").value = "";
    document.getElementById("rule-storage").value = "Ceph";
    document.getElementById("rule-severity").value = "FATAL";
    document.getElementById("rule-pattern").value = "";
    document.getElementById("rule-is-regex").checked = true;
    document.getElementById("rule-file-path-pattern").value = "";
    document.getElementById("rule-desc").value = "";
    document.getElementById("rule-suggestion").value = "";
    this.openModal("modal-rule");
  },

  openEditRuleModal(id) {
    const r = this.rules.find(item => item.id === id);
    if (!r) return;
    document.getElementById("rule-id").value = r.id;
    document.getElementById("rule-name").value = r.name || "";
    document.getElementById("rule-storage").value = r.storage_type || "Generic";
    document.getElementById("rule-severity").value = r.severity || "CRITICAL";
    document.getElementById("rule-pattern").value = r.pattern || "";
    document.getElementById("rule-is-regex").checked = !!r.is_regex;
    document.getElementById("rule-file-path-pattern").value = r.file_path_pattern || "";
    document.getElementById("rule-desc").value = r.description || "";
    document.getElementById("rule-suggestion").value = r.suggestion || "";
    this.openModal("modal-rule");
  },

  async submitRule() {
    const id = document.getElementById("rule-id").value.trim();
    const name = document.getElementById("rule-name").value.trim();
    const storageType = document.getElementById("rule-storage").value;
    const severity = document.getElementById("rule-severity").value;
    const pattern = document.getElementById("rule-pattern").value.trim();
    const isRegex = document.getElementById("rule-is-regex").checked;
    const filePathPattern = document.getElementById("rule-file-path-pattern").value.trim();
    const desc = document.getElementById("rule-desc").value.trim();
    const sugg = document.getElementById("rule-suggestion").value.trim();

    try {
      const payload = {
        name,
        storage_type: storageType,
        severity,
        pattern,
        is_regex: isRegex,
        file_path_pattern: filePathPattern,
        description: desc,
        suggestion: sugg,
        enabled: true,
      };

      const url = id ? `/api/rules/${id}` : "/api/rules";
      const method = id ? "PUT" : "POST";
      const res = await this.api(url, method, payload);
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

  // ================= 导入新安装包一键平滑升级 =================

  openUpgradeModal() {
    const fileInput = document.getElementById("upgrade-file-input");
    if (fileInput) fileInput.value = "";
    const progressBox = document.getElementById("upgrade-progress-box");
    if (progressBox) progressBox.style.display = "none";
    const statusText = document.getElementById("upgrade-status-text");
    if (statusText) {
      statusText.innerText = "准备就绪";
      statusText.style.color = "#38bdf8";
    }
    const progressBar = document.getElementById("upgrade-progress-bar");
    if (progressBar) progressBar.style.width = "0%";
    const percentageText = document.getElementById("upgrade-percentage-text");
    if (percentageText) percentageText.innerText = "0%";
    const detailInfo = document.getElementById("upgrade-detail-info");
    if (detailInfo) {
      detailInfo.innerText = "";
      detailInfo.style.color = "var(--text-dim)";
    }
    const submitBtn = document.getElementById("btn-upgrade-submit");
    if (submitBtn) {
      submitBtn.disabled = false;
      submitBtn.innerText = "🚀 开始上传并自动升级";
    }
    const cancelBtn = document.getElementById("btn-upgrade-cancel");
    if (cancelBtn) cancelBtn.disabled = false;

    this.openModal("modal-upgrade");
  },

  submitUpgradePackage(event) {
    if (event) event.preventDefault();
    const fileInput = document.getElementById("upgrade-file-input");
    if (!fileInput || !fileInput.files || fileInput.files.length === 0) {
      alert("请先选择升级安装包或独立二进制程序");
      return;
    }

    const file = fileInput.files[0];
    if (!confirm(`确认要将管理节点升级为安装包 [${file.name}] 吗？\n升级过程中将自动备份旧版本并执行平滑热重启。`)) {
      return;
    }

    const progressBox = document.getElementById("upgrade-progress-box");
    const statusText = document.getElementById("upgrade-status-text");
    const progressBar = document.getElementById("upgrade-progress-bar");
    const percentageText = document.getElementById("upgrade-percentage-text");
    const detailInfo = document.getElementById("upgrade-detail-info");
    const submitBtn = document.getElementById("btn-upgrade-submit");
    const cancelBtn = document.getElementById("btn-upgrade-cancel");

    if (progressBox) progressBox.style.display = "block";
    if (submitBtn) {
      submitBtn.disabled = true;
      submitBtn.innerText = "⏳ 正在传输升级包...";
    }
    if (cancelBtn) cancelBtn.disabled = true;

    const formData = new FormData();
    formData.append("package", file);

    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/system/upgrade", true);
    if (this.token) {
      xhr.setRequestHeader("Authorization", `Bearer ${this.token}`);
    }

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        const percent = Math.min(99, Math.round((e.loaded / e.total) * 100));
        if (progressBar) progressBar.style.width = `${percent}%`;
        if (percentageText) percentageText.innerText = `${percent}%`;
        if (statusText) statusText.innerText = `正在上传安装包 (${(e.loaded / 1024 / 1024).toFixed(1)}MB / ${(e.total / 1024 / 1024).toFixed(1)}MB)...`;
      }
    };

    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        let resp = {};
        try {
          resp = JSON.parse(xhr.responseText);
        } catch (_) {}

        if (progressBar) progressBar.style.width = "100%";
        if (percentageText) percentageText.innerText = "100%";
        if (statusText) {
          statusText.innerText = "⚡ 安装包解包与架构预检通过，服务重启中...";
          statusText.style.color = "#fbbf24";
        }
        if (detailInfo) {
          detailInfo.innerText = resp.message || "程序已成功原子替换，管理节点正在执行平滑热重启与服务恢复...";
          detailInfo.style.color = "#38bdf8";
        }

        // 进入存活探针轮询
        this.pollHealthAfterUpgrade();
      } else {
        let errMsg = xhr.responseText || "升级失败";
        try {
          const errObj = JSON.parse(xhr.responseText);
          if (errObj.message) errMsg = errObj.message;
        } catch (_) {}

        if (statusText) {
          statusText.innerText = "❌ 升级失败";
          statusText.style.color = "#f87171";
        }
        if (detailInfo) {
          detailInfo.innerText = errMsg;
          detailInfo.style.color = "#f87171";
        }
        if (submitBtn) {
          submitBtn.disabled = false;
          submitBtn.innerText = "重试升级";
        }
        if (cancelBtn) cancelBtn.disabled = false;
        alert("系统升级失败: " + errMsg);
      }
    };

    xhr.onerror = () => {
      if (statusText) {
        statusText.innerText = "🔄 正在探测服务重启状态...";
        statusText.style.color = "#fbbf24";
      }
      this.pollHealthAfterUpgrade();
    };

    xhr.send(formData);
  },

  pollHealthAfterUpgrade() {
    const statusText = document.getElementById("upgrade-status-text");
    const detailInfo = document.getElementById("upgrade-detail-info");
    const progressBar = document.getElementById("upgrade-progress-bar");
    const percentageText = document.getElementById("upgrade-percentage-text");

    let attempts = 0;
    const maxAttempts = 35; // 最多探测 35 次，每次 1.2 秒 (~42秒)

    const checkInterval = setInterval(async () => {
      attempts++;
      if (statusText) {
        statusText.innerText = `🔄 等待服务自愈恢复中 (尝试 ${attempts}/${maxAttempts})...`;
      }

      try {
        const res = await fetch("/healthz?t=" + Date.now(), { cache: "no-store" });
        if (res.ok) {
          clearInterval(checkInterval);
          if (statusText) {
            statusText.innerText = "🎉 系统升级成功，新版本已上线！";
            statusText.style.color = "#34d399";
          }
          if (progressBar) {
            progressBar.style.background = "#34d399";
            progressBar.style.width = "100%";
          }
          if (percentageText) percentageText.innerText = "完成";
          if (detailInfo) {
            detailInfo.innerText = "管理服务已成功重启并恢复健康运行，页面即将自动刷新...";
            detailInfo.style.color = "#34d399";
          }

          setTimeout(() => {
            window.location.reload();
          }, 1500);
          return;
        }
      } catch (_) {
        // 服务尚在重启中，继续轮询
      }

      if (attempts >= maxAttempts) {
        clearInterval(checkInterval);
        if (statusText) {
          statusText.innerText = "⚠️ 自动重连超时";
          statusText.style.color = "#fbbf24";
        }
        if (detailInfo) {
          detailInfo.innerText = "服务可能启动较慢或需要手动检查日志 (systemctl status dist-log-manager)。请稍后手动刷新网页。";
        }
        const cancelBtn = document.getElementById("btn-upgrade-cancel");
        if (cancelBtn) cancelBtn.disabled = false;
      }
    }, 1200);
  },

  // ================= 辅助工具 =================

  openModal(id) {
    const el = document.getElementById(id);
    if (el) el.classList.add("active");
  },

  closeModal(id) {
    const el = document.getElementById(id);
    if (el) el.classList.remove("active");
  },

  closeAllModals() {
    document.querySelectorAll(".modal-overlay.active").forEach(el => {
      el.classList.remove("active");
    });
  },

  // 切换日志浏览窗口最大化/还原 (占满浏览器)
  toggleFileBrowserMaximize() {
    const modal = document.getElementById("modal-file-browser");
    if (!modal) return;
    const isMax = modal.classList.toggle("maximized");
    const icon = document.getElementById("icon-file-browser-maximize");
    const btn = document.getElementById("btn-file-browser-maximize");
    if (isMax) {
      if (icon) icon.innerHTML = "&#x1F5D7;"; // 🗗 还原图标
      if (btn) btn.title = "还原窗口大小 (支持双击标题栏还原)";
      localStorage.setItem("fileBrowserMaximized", "true");
    } else {
      if (icon) icon.innerHTML = "⛶"; // ⛶ 最大化图标
      if (btn) btn.title = "最大化占满浏览器 (支持双击标题栏最大化)";
      localStorage.setItem("fileBrowserMaximized", "false");
    }
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

  isInternalIndexFile(path) {
    if (!path) return false;
    const lower = String(path).toLowerCase();
    return lower.endsWith(".lidx") || lower.endsWith(".bidx") ||
           lower.endsWith(".lidx.tmp") || lower.endsWith(".bidx.tmp");
  },
};

window.addEventListener("DOMContentLoaded", () => app.init());
