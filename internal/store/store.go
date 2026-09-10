package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dist-log-analyzer/internal/config"
	"dist-log-analyzer/internal/model"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/bcrypt"
)

var (
	bucketUsers               = []byte("users")
	bucketNodes               = []byte("nodes")
	bucketArchives            = []byte("archives")
	bucketRules               = []byte("rules")
	bucketReports             = []byte("reports")
	bucketSettings            = []byte("settings")
	bucketAlarms              = []byte("alarms")
	bucketDecommissionedNodes = []byte("decommissioned_nodes")
)

type Store struct {
	db      *bolt.DB
	dataDir string
	mu      sync.RWMutex
}

func NewStore(cfg *config.Config) (*Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data dir: %w", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "analyzer.db")
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open bbolt db: %w", err)
	}

	s := &Store{
		db:      db,
		dataDir: cfg.DataDir,
	}

	// 初始化各 bucket
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketUsers, bucketNodes, bucketArchives, bucketRules, bucketReports, bucketSettings, bucketAlarms, bucketDecommissionedNodes} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	// 初始化管理员账号
	if err := s.initAdmin(cfg.InitialAdmin.Username, cfg.InitialAdmin.Password); err != nil {
		db.Close()
		return nil, err
	}
	// 初始化默认普通业务用户(用于多视图与权限分离体验)
	_ = s.initDefaultUser("user", "user123")

	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ExportSnapshot 导出 bbolt 当前数据快照流 (并发安全)
func (s *Store) ExportSnapshot(w io.Writer) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil {
		return 0, errors.New("database is not open")
	}

	var written int64
	err := s.db.View(func(tx *bolt.Tx) error {
		n, err := tx.WriteTo(w)
		written = n
		return err
	})
	return written, err
}

// ReloadFromSnapshot 从快照流原子重载数据库 (备节点数据同步专用)
func (s *Store) ReloadFromSnapshot(r io.Reader) (int64, error) {
	tmpPath := filepath.Join(s.dataDir, "analyzer.db.sync_tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, fmt.Errorf("创建同步临时文件失败: %w", err)
	}

	written, err := io.Copy(f, r)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("写入同步数据流失败: %w", err)
	}

	// 验证临时数据库文件是否完好
	testDB, err := bolt.Open(tmpPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("验证同步数据快照损坏: %w", err)
	}
	_ = testDB.Close()

	// 加全局排他写锁，执行原子切换
	s.mu.Lock()
	defer s.mu.Unlock()

	dbPath := filepath.Join(s.dataDir, "analyzer.db")
	if s.db != nil {
		_ = s.db.Close()
	}

	if err := os.Rename(tmpPath, dbPath); err != nil {
		// rename 失败尝试重新打开原 db
		s.db, _ = bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
		return 0, fmt.Errorf("原子替换数据库文件失败: %w", err)
	}

	newDB, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return 0, fmt.Errorf("重新加载数据库失败: %w", err)
	}
	s.db = newDB

	return written, nil
}

func (s *Store) initAdmin(username, password string) error {
	existing, err := s.GetUserByUsername(username)
	if err == nil && existing != nil {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	admin := &model.User{
		ID:               "usr_admin_001",
		Username:         username,
		PasswordHash:     string(hash),
		Role:             model.RoleAdmin,
		SpaceQuotaBytes:  0, // 不受限制
		UsedStorageBytes: 0,
		Status:           "active",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.SaveUser(admin); err != nil {
		return err
	}
	// 创建物理目录
	return s.EnsureUserDirectories(username)
}

func (s *Store) initDefaultUser(username, password string) error {
	existing, err := s.GetUserByUsername(username)
	if err == nil && existing != nil {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u := &model.User{
		ID:               "usr_default_002",
		Username:         username,
		PasswordHash:     string(hash),
		Role:             model.RoleUser,
		SpaceQuotaBytes:  10 * 1024 * 1024 * 1024, // 默认 10GB 配额
		UsedStorageBytes: 0,
		Status:           "active",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.SaveUser(u); err != nil {
		return err
	}
	return s.EnsureUserDirectories(username)
}

// ================= 用户物理存储空间隔离 =================

func (s *Store) GetUserBaseDir(username string) string {
	return filepath.Join(s.dataDir, "users", username)
}

func (s *Store) GetUserArchiveDir(username string) string {
	return filepath.Join(s.GetUserBaseDir(username), "archives")
}

func (s *Store) GetUserExtractDir(username, archiveID string) string {
	return filepath.Join(s.GetUserBaseDir(username), "extracted", archiveID)
}

func (s *Store) EnsureUserDirectories(username string) error {
	if err := os.MkdirAll(s.GetUserArchiveDir(username), 0755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(s.GetUserBaseDir(username), "extracted"), 0755)
}

// CalculateUserStorageUsage 实时遍历用户物理目录统计使用空间
func (s *Store) CalculateUserStorageUsage(username string) (int64, error) {
	baseDir := s.GetUserBaseDir(username)
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return 0, nil
	}
	var totalSize int64
	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err == nil {
				totalSize += info.Size()
			}
		}
		return nil
	})
	return totalSize, err
}

// CheckUserQuota 校验用户存储配额
func (s *Store) CheckUserQuota(username string, incomingSize int64) error {
	user, err := s.GetUserByUsername(username)
	if err != nil {
		return err
	}
	if user.SpaceQuotaBytes <= 0 {
		return nil // 无限制
	}
	usage, err := s.CalculateUserStorageUsage(username)
	if err != nil {
		return err
	}
	if usage+incomingSize > user.SpaceQuotaBytes {
		return fmt.Errorf("存储空间配额不足: 当前使用 %.2f MB, 尝试增加 %.2f MB, 配额上限 %.2f MB",
			float64(usage)/(1024*1024), float64(incomingSize)/(1024*1024), float64(user.SpaceQuotaBytes)/(1024*1024))
	}
	return nil
}

// ================= 用户数据存取 =================

func (s *Store) SaveUser(u *model.User) error {
	u.UpdatedAt = time.Now()
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.Put([]byte(u.Username), data)
	})
}

func (s *Store) GetUserByUsername(username string) (*model.User, error) {
	var u *model.User
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		data := b.Get([]byte(username))
		if data == nil {
			return errors.New("user not found")
		}
		return json.Unmarshal(data, &u)
	})
	return u, err
}

func (s *Store) ListUsers() ([]*model.User, error) {
	var list []*model.User
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.ForEach(func(k, v []byte) error {
			var u model.User
			if err := json.Unmarshal(v, &u); err == nil {
				list = append(list, &u)
			}
			return nil
		})
	})
	// 动态更新已用空间
	for _, u := range list {
		if usage, err := s.CalculateUserStorageUsage(u.Username); err == nil {
			u.UsedStorageBytes = usage
		}
	}
	return list, err
}

func (s *Store) DeleteUser(username string) error {
	if username == "admin" {
		return errors.New("cannot delete default admin")
	}
	// 清理物理目录
	_ = os.RemoveAll(s.GetUserBaseDir(username))
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.Delete([]byte(username))
	})
}

// ================= 节点管理 =================

func (s *Store) SaveNode(n *model.Node) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		return b.Put([]byte(n.ID), data)
	})
}

func (s *Store) GetNode(id string) (*model.Node, error) {
	var n *model.Node
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		data := b.Get([]byte(id))
		if data == nil {
			return errors.New("node not found")
		}
		return json.Unmarshal(data, &n)
	})
	return n, err
}

// GetNodeByAddr 根据 IP 和 Port 查询业务组件节点 (以 IP+端口 作为唯一标识)
func (s *Store) GetNodeByAddr(ip string, port int) (*model.Node, error) {
	var matched *model.Node
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return errors.New("bucket not found")
		}
		return b.ForEach(func(k, v []byte) error {
			var n model.Node
			if err := json.Unmarshal(v, &n); err == nil {
				if n.Role == "worker" && n.IP == ip && n.Port == port {
					matched = &n
					return errors.New("found")
				}
			}
			return nil
		})
	})
	if matched != nil {
		if usage, err := s.CalculateNodeStorageUsage(matched.ID); err == nil {
			matched.StorageUsedBytes = usage
		}
		return matched, nil
	}
	if err != nil && err.Error() != "found" {
		return nil, err
	}
	return nil, errors.New("node not found")
}

// CalculateNodeStorageUsage 统计指定业务节点上保存的日志归档总字节数
func (s *Store) CalculateNodeStorageUsage(nodeID string) (int64, error) {
	var totalBytes int64
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var a model.LogArchive
			if err := json.Unmarshal(v, &a); err == nil {
				if a.StorageNodeID == nodeID {
					totalBytes += a.Size
				}
			}
			return nil
		})
	})
	return totalBytes, err
}

// ListArchivesByNode 查询属于指定节点的所有日志归档包
func (s *Store) ListArchivesByNode(nodeID string) ([]*model.LogArchive, error) {
	var list []*model.LogArchive
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var a model.LogArchive
			if err := json.Unmarshal(v, &a); err == nil {
				if a.StorageNodeID == nodeID {
					list = append(list, &a)
				}
			}
			return nil
		})
	})
	return list, err
}

func (s *Store) ListNodes() ([]*model.Node, error) {
	var list []*model.Node
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		return b.ForEach(func(k, v []byte) error {
			var n model.Node
			if err := json.Unmarshal(v, &n); err == nil {
				list = append(list, &n)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// 动态填充各节点的存储日志大小
	for _, n := range list {
		if usage, err := s.CalculateNodeStorageUsage(n.ID); err == nil {
			n.StorageUsedBytes = usage
		}
	}
	return list, nil
}

func (s *Store) DeleteNode(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		return b.Delete([]byte(id))
	})
}

// DecommissionNode 将节点从活跃列表删除并写入注销/下线黑名单，防止其心跳自动复活
func (s *Store) DecommissionNode(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		_ = tx.Bucket(bucketNodes).Delete([]byte(id))
		b := tx.Bucket(bucketDecommissionedNodes)
		if b != nil {
			return b.Put([]byte(id), []byte(time.Now().Format(time.RFC3339)))
		}
		return nil
	})
}

// IsNodeDecommissioned 检查节点是否属于已注销节点
func (s *Store) IsNodeDecommissioned(id string) bool {
	var decommissioned bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDecommissionedNodes)
		if b != nil && b.Get([]byte(id)) != nil {
			decommissioned = true
		}
		return nil
	})
	return decommissioned
}

// ClearDecommissionedNode 清理节点的注销标记（当用户显式重新部署/添加该节点时调用）
func (s *Store) ClearDecommissionedNode(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDecommissionedNodes)
		if b != nil {
			return b.Delete([]byte(id))
		}
		return nil
	})
}

// ================= 日志压缩包存取 =================

func (s *Store) SaveArchive(a *model.LogArchive) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		return b.Put([]byte(a.ID), data)
	})
}

func (s *Store) GetArchive(id string) (*model.LogArchive, error) {
	var a *model.LogArchive
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		data := b.Get([]byte(id))
		if data == nil {
			return errors.New("archive not found")
		}
		return json.Unmarshal(data, &a)
	})
	return a, err
}

func (s *Store) ListArchives(username string, isAdmin bool) ([]*model.LogArchive, error) {
	var list []*model.LogArchive
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		return b.ForEach(func(k, v []byte) error {
			var a model.LogArchive
			if err := json.Unmarshal(v, &a); err == nil {
				if isAdmin || a.Username == username {
					list = append(list, &a)
				}
			}
			return nil
		})
	})
	return list, err
}

func (s *Store) DeleteArchive(id, username string, isAdmin bool) error {
	a, err := s.GetArchive(id)
	if err != nil {
		return err
	}
	if !isAdmin && a.Username != username {
		return errors.New("permission denied")
	}
	// 1. 删除物理文件及解包目录
	if a.ExtractPath != "" {
		_ = os.RemoveAll(a.ExtractPath)
		if a.Filename != "" {
			workerArchive := filepath.Join(filepath.Dir(filepath.Dir(a.ExtractPath)), "archives", a.Filename)
			_ = os.Remove(workerArchive)
		}
	}
	if a.Filename != "" {
		archiveFile := filepath.Join(s.GetUserArchiveDir(a.Username), a.Filename)
		_ = os.Remove(archiveFile)
	}

	// 2. 删除关联的诊断报告
	_ = s.DeleteReport(id)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketArchives)
		return b.Delete([]byte(id))
	})
}

// ================= 规则管理 =================

func (s *Store) SaveRule(r *model.Rule) error {
	r.UpdatedAt = time.Now()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRules)
		return b.Put([]byte(r.ID), data)
	})
}

func (s *Store) GetRule(id string) (*model.Rule, error) {
	var r *model.Rule
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRules)
		data := b.Get([]byte(id))
		if data == nil {
			return errors.New("rule not found")
		}
		return json.Unmarshal(data, &r)
	})
	return r, err
}

func (s *Store) ListRules() ([]*model.Rule, error) {
	var list []*model.Rule
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRules)
		return b.ForEach(func(k, v []byte) error {
			var r model.Rule
			if err := json.Unmarshal(v, &r); err == nil {
				list = append(list, &r)
			}
			return nil
		})
	})
	return list, err
}

func (s *Store) DeleteRule(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRules)
		return b.Delete([]byte(id))
	})
}

// ================= 诊断报告 =================

func (s *Store) SaveReport(rep *model.DiagnosisReport) error {
	data, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReports)
		return b.Put([]byte(rep.ArchiveID), data)
	})
}

func (s *Store) GetReport(archiveID string) (*model.DiagnosisReport, error) {
	var rep *model.DiagnosisReport
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReports)
		data := b.Get([]byte(archiveID))
		if data == nil {
			return errors.New("report not found")
		}
		return json.Unmarshal(data, &rep)
	})
	return rep, err
}

func (s *Store) DeleteReport(archiveID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReports)
		return b.Delete([]byte(archiveID))
	})
}

// ================= 系统设置与高可用网络配置 =================

var keyHAConfig = []byte("ha_config")

// SaveHAConfig 持久化保存高可用与网络配置
func (s *Store) SaveHAConfig(cfg *config.HAConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSettings)
		return b.Put(keyHAConfig, data)
	})
}

// GetHAConfig 读取持久化的高可用与网络配置
func (s *Store) GetHAConfig() (*config.HAConfig, error) {
	var cfg *config.HAConfig
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSettings)
		data := b.Get(keyHAConfig)
		if data == nil {
			return errors.New("ha_config not set")
		}
		return json.Unmarshal(data, &cfg)
	})
	return cfg, err
}

// ================= 告警数据存取与生命周期管理 =================

// SaveAlarm 保存告警实体
func (s *Store) SaveAlarm(a *model.Alarm) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.Put([]byte(a.ID), data)
	})
}

// GetAlarm 根据 ID 获取告警
func (s *Store) GetAlarm(id string) (*model.Alarm, error) {
	var a *model.Alarm
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		data := b.Get([]byte(id))
		if data == nil {
			return errors.New("alarm not found")
		}
		return json.Unmarshal(data, &a)
	})
	return a, err
}

// FindActiveAlarm 查找指定节点上尚未解除的同类型活跃告警 (用于聚合去重)
func (s *Store) FindActiveAlarm(nodeID, alarmType string) (*model.Alarm, error) {
	var target *model.Alarm
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.ForEach(func(k, v []byte) error {
			var a model.Alarm
			if err := json.Unmarshal(v, &a); err == nil {
				if a.NodeID == nodeID && a.AlarmType == alarmType && a.Status != model.AlarmStatusResolved {
					target = &a
					return nil
				}
			}
			return nil
		})
	})
	if target == nil {
		return nil, errors.New("active alarm not found")
	}
	return target, err
}

// CreateOrAggregateAlarm 创建新告警，或对已存在的活跃告警执行频次累加与更新 (防止告警风暴)
func (s *Store) CreateOrAggregateAlarm(in *model.Alarm) (*model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, _ := s.FindActiveAlarm(in.NodeID, in.AlarmType)
	if existing != nil {
		// 已存在活跃告警，累加频次并更新最新时间与说明
		existing.Count++
		existing.LastOccurAt = time.Now()
		existing.Message = in.Message
		if in.Severity != "" {
			existing.Severity = in.Severity
		}
		if in.Title != "" {
			existing.Title = in.Title
		}
		if err := s.SaveAlarm(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// 首次发生，生成新 ID
	if in.ID == "" {
		in.ID = fmt.Sprintf("alm_%d_%s", time.Now().UnixNano()/1e6, in.AlarmType)
	}
	if in.Status == "" {
		in.Status = model.AlarmStatusActive
	}
	if in.Count <= 0 {
		in.Count = 1
	}
	now := time.Now()
	if in.FirstOccurAt.IsZero() {
		in.FirstOccurAt = now
	}
	in.LastOccurAt = now

	if err := s.SaveAlarm(in); err != nil {
		return nil, err
	}
	return in, nil
}

// ResolveAlarm 自动消警：将指定节点和类型的未恢复告警标记为 resolved
func (s *Store) ResolveAlarm(nodeID, alarmType string) (*model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.FindActiveAlarm(nodeID, alarmType)
	if err != nil || existing == nil {
		return nil, nil // 无活跃告警，无需消警
	}

	now := time.Now()
	existing.Status = model.AlarmStatusResolved
	existing.ResolvedAt = &now
	if err := s.SaveAlarm(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// ListAlarms 查询告警列表 (按最后发生时间倒序)，支持 statusFilter (active / resolved / all)
func (s *Store) ListAlarms(statusFilter string) ([]*model.Alarm, error) {
	var list []*model.Alarm
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.ForEach(func(k, v []byte) error {
			var a model.Alarm
			if err := json.Unmarshal(v, &a); err == nil {
				if statusFilter == "" || statusFilter == "all" || a.Status == statusFilter || (statusFilter == "active" && a.Status != model.AlarmStatusResolved) {
					list = append(list, &a)
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// 内存按 LastOccurAt 降序排序
	for i := 0; i < len(list)-1; i++ {
		for j := i + 1; j < len(list); j++ {
			if list[i].LastOccurAt.Before(list[j].LastOccurAt) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	return list, nil
}

// AcknowledgeAlarm 管理员确认告警
func (s *Store) AcknowledgeAlarm(id string) error {
	a, err := s.GetAlarm(id)
	if err != nil {
		return err
	}
	if a.Status == model.AlarmStatusResolved {
		return errors.New("已恢复的告警无需确认")
	}
	a.Status = model.AlarmStatusAcknowledged
	return s.SaveAlarm(a)
}

// ManualResolveAlarm 管理员手动消警
func (s *Store) ManualResolveAlarm(id string) error {
	a, err := s.GetAlarm(id)
	if err != nil {
		return err
	}
	now := time.Now()
	a.Status = model.AlarmStatusResolved
	a.ResolvedAt = &now
	return s.SaveAlarm(a)
}

// DeleteAlarm 删除一条告警
func (s *Store) DeleteAlarm(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.Delete([]byte(id))
	})
}

// ClearResolvedAlarms 清理全部已恢复的历史告警
func (s *Store) ClearResolvedAlarms() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var toDelete [][]byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.ForEach(func(k, v []byte) error {
			var a model.Alarm
			if err := json.Unmarshal(v, &a); err == nil {
				if a.Status == model.AlarmStatusResolved {
					toDelete = append(toDelete, k)
				}
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}

	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		for _, k := range toDelete {
			_ = b.Delete(k)
		}
		return nil
	})
	return len(toDelete), err
}

// GetAlarmSummary 统计活跃告警数据
func (s *Store) GetAlarmSummary() model.AlarmSummary {
	var sum model.AlarmSummary
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAlarms)
		return b.ForEach(func(k, v []byte) error {
			var a model.Alarm
			if err := json.Unmarshal(v, &a); err == nil {
				if a.Status != model.AlarmStatusResolved {
					sum.TotalActive++
					switch a.Severity {
					case model.SeverityCritical:
						sum.CriticalCount++
					case model.SeverityWarning:
						sum.WarningCount++
					default:
						sum.InfoCount++
					}
				}
			}
			return nil
		})
	})
	return sum
}

