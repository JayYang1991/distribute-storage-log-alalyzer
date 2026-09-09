package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"
	"dist-log-analyzer/internal/store"
)

func TestRoleBasedAccessControl(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "role_view_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.DataDir = tempDir
	cfg.InitialAdmin.Username = "admin"
	cfg.InitialAdmin.Password = "admin123"

	st, err := store.NewStore(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer st.Close()

	server := NewServer(cfg, st, nil)

	// 1. 验证默认初始化的两个角色账号登录
	// 1.1 管理员登录
	adminLoginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
	})
	adminLoginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(adminLoginBody))
	adminLoginReq.Header.Set("Content-Type", "application/json")
	adminLoginRec := httptest.NewRecorder()
	server.handleLogin(adminLoginRec, adminLoginReq)

	if adminLoginRec.Code != http.StatusOK {
		t.Fatalf("Admin login failed: %d, body: %s", adminLoginRec.Code, adminLoginRec.Body.String())
	}
	var adminResp struct {
		Token string     `json:"token"`
		User  model.User `json:"user"`
	}
	_ = json.Unmarshal(adminLoginRec.Body.Bytes(), &adminResp)
	if adminResp.User.Role != model.RoleAdmin {
		t.Fatalf("Expected admin role, got %s", adminResp.User.Role)
	}

	// 1.2 普通用户登录 (系统自动初始化的 user/user123)
	userLoginBody, _ := json.Marshal(map[string]string{
		"username": "user",
		"password": "user123",
	})
	userLoginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(userLoginBody))
	userLoginReq.Header.Set("Content-Type", "application/json")
	userLoginRec := httptest.NewRecorder()
	server.handleLogin(userLoginRec, userLoginReq)

	if userLoginRec.Code != http.StatusOK {
		t.Fatalf("User login failed: %d, body: %s", userLoginRec.Code, userLoginRec.Body.String())
	}
	var userResp struct {
		Token string     `json:"token"`
		User  model.User `json:"user"`
	}
	_ = json.Unmarshal(userLoginRec.Body.Bytes(), &userResp)
	if userResp.User.Role != model.RoleUser {
		t.Fatalf("Expected user role, got %s", userResp.User.Role)
	}

	userAuthHeader := "Bearer " + userResp.Token
	adminAuthHeader := "Bearer " + adminResp.Token

	// 2. 验证普通用户无法访问配置类接口 (不支持配置 - 403 Forbidden 拦截)
	// 2.1 规则配置添加: POST /api/rules
	rulePayload, _ := json.Marshal(model.Rule{
		Name:    "Test Rule",
		Pattern: "test.*",
	})
	userRuleReq := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(rulePayload))
	userRuleReq.Header.Set("Authorization", userAuthHeader)
	userRuleReq.Header.Set("Content-Type", "application/json")
	userRuleRec := httptest.NewRecorder()
	server.handleRules(userRuleRec, userRuleReq)

	if userRuleRec.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden when regular user tries to configure rule, got %d", userRuleRec.Code)
	}

	// 2.2 用户管理与存储配额配置: GET /api/users
	userListReq := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	userListReq.Header.Set("Authorization", userAuthHeader)
	userListRec := httptest.NewRecorder()
	server.handleUsers(userListRec, userListReq)

	if userListRec.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden when regular user accesses user management, got %d", userListRec.Code)
	}

	// 2.3 高可用 HA 配置: POST /api/ha/config
	haCfgPayload, _ := json.Marshal(map[string]interface{}{
		"mode": "standalone",
	})
	userHAReq := httptest.NewRequest(http.MethodPost, "/api/ha/config", bytes.NewReader(haCfgPayload))
	userHAReq.Header.Set("Authorization", userAuthHeader)
	userHAReq.Header.Set("Content-Type", "application/json")
	userHARec := httptest.NewRecorder()
	server.handleHAConfig(userHARec, userHAReq)

	if userHARec.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden when regular user updates HA config, got %d", userHARec.Code)
	}

	// 2.4 集群节点一键部署: POST /api/nodes/deploy
	deployPayload, _ := json.Marshal(map[string]interface{}{
		"host": "192.168.1.50",
	})
	userDeployReq := httptest.NewRequest(http.MethodPost, "/api/nodes/deploy", bytes.NewReader(deployPayload))
	userDeployReq.Header.Set("Authorization", userAuthHeader)
	userDeployReq.Header.Set("Content-Type", "application/json")
	userDeployRec := httptest.NewRecorder()
	server.handleDeployWorker(userDeployRec, userDeployReq)

	if userDeployRec.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden when regular user triggers node deploy, got %d", userDeployRec.Code)
	}

	// 3. 验证普通用户可以正常访问日志上传与归档分析相关接口
	// 3.1 查询归档列表 (应返回 200 OK)
	userArchReq := httptest.NewRequest(http.MethodGet, "/api/archives", nil)
	userArchReq.Header.Set("Authorization", userAuthHeader)
	userArchRec := httptest.NewRecorder()
	server.handleArchives(userArchRec, userArchReq)

	if userArchRec.Code != http.StatusOK {
		t.Errorf("Expected 200 OK when regular user accesses archives, got %d", userArchRec.Code)
	}

	// 4. 验证管理员具备全量配置权限 (不会被 403 拦截)
	// 4.1 管理员访问用户管理: GET /api/users 应返回 200 OK
	adminListReq := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	adminListReq.Header.Set("Authorization", adminAuthHeader)
	adminListRec := httptest.NewRecorder()
	server.handleUsers(adminListRec, adminListReq)

	if adminListRec.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for admin accessing users, got %d", adminListRec.Code)
	}

	// 4.2 管理员添加规则: POST /api/rules 应返回 200 OK
	adminRuleReq := httptest.NewRequest(http.MethodPost, "/api/rules", bytes.NewReader(rulePayload))
	adminRuleReq.Header.Set("Authorization", adminAuthHeader)
	adminRuleReq.Header.Set("Content-Type", "application/json")
	adminRuleRec := httptest.NewRecorder()
	server.handleRules(adminRuleRec, adminRuleReq)

	if adminRuleRec.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for admin creating rule, got %d", adminRuleRec.Code)
	}

	t.Log("✔ 角色与视图权限隔离单元测试全量验证通过！")
}
