package manager

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"
)

// DeleteArchiveStorageAndMeta 彻底清除指定归档包的物理存储、解压目录及元数据
func (s *Server) DeleteArchiveStorageAndMeta(archive *model.LogArchive) error {
	if archive == nil {
		return fmt.Errorf("archive is nil")
	}

	// 1. 通知远端存储节点 Worker 清理物理存储和解压目录
	if archive.StorageNodeIP != "" && archive.StorageNodePort > 0 && archive.StorageNodeID != "manager_primary" {
		cleanURL := fmt.Sprintf("http://%s:%d/api/worker/storage/clean-archive?username=%s&archive_id=%s&filename=%s&extract_path=%s",
			archive.StorageNodeIP,
			archive.StorageNodePort,
			url.QueryEscape(archive.Username),
			url.QueryEscape(archive.ID),
			url.QueryEscape(archive.Filename),
			url.QueryEscape(archive.ExtractPath),
		)
		client := &http.Client{Timeout: 15 * time.Second}
		req, reqErr := http.NewRequest(http.MethodDelete, cleanURL, nil)
		if reqErr == nil {
			resp, respErr := client.Do(req)
			if respErr == nil {
				_ = resp.Body.Close()
				log.Printf("[Storage Cleaner] 已成功通知业务节点 %s (%s:%d) 清理日志包 %s 的磁盘空间",
					archive.StorageNodeName, archive.StorageNodeIP, archive.StorageNodePort, archive.Filename)
			} else {
				log.Printf("[Storage Cleaner] 通知业务节点清理日志包失败: %v，将尝试本地清理", respErr)
			}
		}
	}

	// 2. 本地直接清理兜底 (针对本地挂载、管理节点存储或同机部署场景)
	if archive.ExtractPath != "" {
		_ = os.RemoveAll(archive.ExtractPath)
		workerArchiveFile := filepath.Join(filepath.Dir(filepath.Dir(archive.ExtractPath)), "archives", archive.Filename)
		if _, statErr := os.Stat(workerArchiveFile); statErr == nil {
			_ = os.Remove(workerArchiveFile)
		}
	}

	// 3. 数据库元数据与诊断报告清理 (isAdmin=true)
	return s.store.DeleteArchive(archive.ID, archive.Username, true)
}

// StartRetentionLoop 启动后台日志生命周期与磁盘容量自愈协程
func (s *Server) StartRetentionLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	// 启动后稍作延迟即执行首次检查
	time.AfterFunc(10*time.Second, func() {
		s.RunRetentionCycle()
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunRetentionCycle()
		}
	}
}

// IsArchiveExemptFromAutoClean 判断归档日志包是否免除系统自动清理 (受管理员指导与保护)
// 命中以下任意一条规则即享有绝对免清理保护：
// 1. 管理员显式锁定了单包 (arc.Pinned == true)
// 2. 归档包所带业务标签命中了管理员在生命周期设置中配置的免清理白名单标签 (cfg.ExemptTags)
func IsArchiveExemptFromAutoClean(arc *model.LogArchive, cfg *model.RetentionConfig) bool {
	if arc == nil {
		return true
	}
	// 1. 单包保护锁定
	if arc.Pinned {
		return true
	}
	// 2. 管理员设置指导的免清理标签匹配
	if cfg != nil && len(cfg.ExemptTags) > 0 && len(arc.Tags) > 0 {
		for _, tag := range arc.Tags {
			t := strings.TrimSpace(tag)
			if t == "" {
				continue
			}
			for _, exempt := range cfg.ExemptTags {
				ex := strings.TrimSpace(exempt)
				if ex != "" && strings.EqualFold(t, ex) {
					return true
				}
			}
		}
	}
	return false
}

// RunRetentionCycle 执行单次生命周期与水位自愈巡检
func (s *Server) RunRetentionCycle() {
	cfg, err := s.store.GetRetentionConfig()
	if err != nil || cfg == nil || !cfg.AutoCleanEnabled {
		return
	}

	// 1. TTL 生命周期检查与清理 (默认 180 天 / 6 个月)
	if cfg.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
		archives, err := s.store.ListArchives("", true)
		if err == nil {
			for _, arc := range archives {
				if !IsArchiveExemptFromAutoClean(arc, cfg) && arc.UploadTime.Before(cutoff) {
					log.Printf("[Retention] 日志包 [%s] (ID: %s, 上传时间: %s) 超过保留期 %d 天，触发自动清理",
						arc.Filename, arc.ID, arc.UploadTime.Format("2006-01-02 15:04:05"), cfg.RetentionDays)
					if cleanErr := s.DeleteArchiveStorageAndMeta(arc); cleanErr != nil {
						log.Printf("[Retention] 自动清理过期日志包 [%s] 失败: %v", arc.Filename, cleanErr)
					}
				}
			}
		}
	}

	// 2. 磁盘紧急水位保护与自愈清理
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}

	for _, node := range nodes {
		if node.Status != "online" || node.Resource.DiskUsedPercent <= 0 {
			continue
		}

		// 判断是否达到紧急清理水位
		emergencyThreshold := float64(cfg.EmergencyWatermarkPercent)
		if emergencyThreshold <= 0 {
			emergencyThreshold = 92.0
		}
		targetThreshold := float64(cfg.TargetWatermarkPercent)
		if targetThreshold <= 0 {
			targetThreshold = 75.0
		}

		if node.Resource.DiskUsedPercent >= emergencyThreshold {
			log.Printf("[Capacity Self-Healing] ⚠️ 节点 [%s] 磁盘使用率达到 %.1f%%，超过紧急水位 %.1f%%，启动自愈清理！",
				node.Name, node.Resource.DiskUsedPercent, emergencyThreshold)

			// 找出存放在该节点的所有未受保护归档包，按 UploadTime 升序 (最旧的优先)
			nodeArchives, aErr := s.store.ListArchivesByNode(node.ID)
			if aErr != nil || len(nodeArchives) == 0 {
				continue
			}

			var candidates []*model.LogArchive
			for _, arc := range nodeArchives {
				if !IsArchiveExemptFromAutoClean(arc, cfg) {
					candidates = append(candidates, arc)
				}
			}

			sort.Slice(candidates, func(i, j int) bool {
				return candidates[i].UploadTime.Before(candidates[j].UploadTime)
			})

			currentUsedMB := node.Resource.DiskUsedMB
			totalMB := node.Resource.DiskTotalMB
			if totalMB <= 0 {
				continue
			}

			cleanedCount := 0
			for _, arc := range candidates {
				usedPercent := float64(currentUsedMB) / float64(totalMB) * 100
				if usedPercent <= targetThreshold {
					log.Printf("[Capacity Self-Healing] ✔ 节点 [%s] 磁盘使用率已回落至安全水位 %.1f%% (目标: %.1f%%)",
						node.Name, usedPercent, targetThreshold)
					break
				}

				log.Printf("[Capacity Self-Healing] 正在清理该节点历史日志包 [%s] (大小: %d MB)...",
					arc.Filename, arc.Size/(1024*1024))
				if err := s.DeleteArchiveStorageAndMeta(arc); err == nil {
					cleanedCount++
					currentUsedMB -= arc.Size / (1024 * 1024)
					if currentUsedMB < 0 {
						currentUsedMB = 0
					}
				}
			}

			log.Printf("[Capacity Self-Healing] 节点 [%s] 本轮自愈完成，共清理 %d 个日志包", node.Name, cleanedCount)
		}
	}
}
