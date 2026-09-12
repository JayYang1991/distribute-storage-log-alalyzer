package indexer

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"dist-log-analyzer/internal/model"
)

var (
	reTimestamp = regexp.MustCompile(`^(\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:?\d{2}|Z)?|\[\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?\]|[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2})`)
	reLogLevel  = regexp.MustCompile(`(?i)\b(FATAL|CRITICAL|ERROR|WARN(?:ING)?|INFO|DEBUG|TRACE)\b`)
	reIPv4Port  = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?::\d+)?\b`)
	reUUID      = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reHex       = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	reDigits    = regexp.MustCompile(`\b\d+\b`)
	reQuoted    = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)
	reTraceID   = regexp.MustCompile(`(?i)\b(?:trace_?id|request_?id|req_?id|span_?id)[=:]\s*([a-zA-Z0-9_-]+)`)
)

// DrainNode 前缀树节点
type DrainNode struct {
	Children map[string]*DrainNode
	Clusters []*Cluster
}

// Cluster 模板簇
type Cluster struct {
	ID        string
	Tokens    []string
	Sample    string
	Count     int64
	Level     string
	FirstSeen string
	LastSeen  string
	Files     map[string]bool
}

// DrainMiner 日志模式挖掘聚类器
type DrainMiner struct {
	mu           sync.Mutex
	root         *DrainNode
	maxDepth     int
	simThreshold float64
	maxClusters  int
	clusters     []*Cluster
}

// NewDrainMiner 创建 Drain 挖掘器
func NewDrainMiner(simThreshold float64, maxDepth int) *DrainMiner {
	if simThreshold <= 0 {
		simThreshold = 0.55
	}
	if maxDepth <= 0 {
		maxDepth = 4
	}
	return &DrainMiner{
		root:         &DrainNode{Children: make(map[string]*DrainNode)},
		maxDepth:     maxDepth,
		simThreshold: simThreshold,
		maxClusters:  1000,
	}
}

// PreprocessLog 清洗原始单行日志，剥离时间戳、提取级别并泛化动态变量
func PreprocessLog(raw string) (cleaned string, level string, ts string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "INFO", ""
	}

	// 1. 提取并剥离时间戳
	if loc := reTimestamp.FindStringIndex(s); loc != nil && loc[0] == 0 {
		ts = strings.Trim(s[:loc[1]], "[]")
		s = strings.TrimSpace(s[loc[1]:])
	}

	// 2. 提取日志级别
	level = "INFO"
	if match := reLogLevel.FindString(s); match != "" {
		u := strings.ToUpper(match)
		if strings.HasPrefix(u, "WARN") {
			level = "WARN"
		} else if strings.HasPrefix(u, "ERR") {
			level = "ERROR"
		} else if u == "FATAL" || u == "CRITICAL" {
			level = "FATAL"
		} else if u == "INFO" || u == "DEBUG" || u == "TRACE" {
			level = u
		}
	}

	// 3. 通用变量通配替换 (前置字符快速短路跳过 + 高性能词法扫描)
	if strings.IndexByte(s, '.') != -1 {
		s = reIPv4Port.ReplaceAllString(s, "<*>")
	}
	if strings.IndexByte(s, '-') != -1 {
		s = reUUID.ReplaceAllString(s, "<*>")
	}
	if strings.Contains(s, "0x") || strings.Contains(s, "0X") {
		s = reHex.ReplaceAllString(s, "<*>")
	}
	if strings.IndexByte(s, '"') != -1 || strings.IndexByte(s, '\'') != -1 {
		s = reQuoted.ReplaceAllString(s, `"<*>"`)
	}
	s = fastReplaceDigits(s)

	return s, level, ts
}

func fastReplaceDigits(s string) string {
	n := len(s)
	if n == 0 {
		return s
	}

	hasDigit := false
	for i := 0; i < n; i++ {
		if s[i] >= '0' && s[i] <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return s
	}

	var buf strings.Builder
	buf.Grow(n)

	i := 0
	for i < n {
		b := s[i]
		if b >= '0' && b <= '9' {
			isWordBoundaryLeft := (i == 0) || !isWordByte(s[i-1])
			start := i
			for i < n && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			isWordBoundaryRight := (i == n) || !isWordByte(s[i])

			if isWordBoundaryLeft && isWordBoundaryRight {
				buf.WriteString("<*>")
			} else {
				buf.WriteString(s[start:i])
			}
		} else {
			buf.WriteByte(b)
			i++
		}
	}
	return buf.String()
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// AddLog 向聚类器投递一条日志
func (d *DrainMiner) AddLog(raw string, file string) {
	cleaned, level, ts := PreprocessLog(raw)
	if cleaned == "" {
		return
	}

	// 按空白切分 Token
	rawTokens := strings.Fields(cleaned)
	if len(rawTokens) == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. 树状导航查找候选簇
	cluster := d.treeSearch(d.root, rawTokens)

	if cluster != nil {
		// 命中已有聚类，更新通配模板与频次
		cluster.Count++
		cluster.LastSeen = ts
		if file != "" {
			cluster.Files[file] = true
		}
		if levelPriority(level) > levelPriority(cluster.Level) {
			cluster.Level = level
		}
		// 若新样本在某位置存在差异，将该位置转为通配符
		minLen := len(cluster.Tokens)
		if len(rawTokens) < minLen {
			minLen = len(rawTokens)
		}
		for i := 0; i < minLen; i++ {
			if cluster.Tokens[i] != rawTokens[i] {
				cluster.Tokens[i] = "<*>"
			}
		}
	} else {
		// 创建新簇
		if len(d.clusters) >= d.maxClusters {
			return
		}
		clusterID := fmt.Sprintf("tpl_%x", md5.Sum([]byte(strings.Join(rawTokens, " "))))[:10]
		newCluster := &Cluster{
			ID:        clusterID,
			Tokens:    rawTokens,
			Sample:    raw,
			Count:     1,
			Level:     level,
			FirstSeen: ts,
			LastSeen:  ts,
			Files:     make(map[string]bool),
		}
		if file != "" {
			newCluster.Files[file] = true
		}
		d.clusters = append(d.clusters, newCluster)
		d.treeAdd(d.root, newCluster)
	}
}

func levelPriority(lvl string) int {
	switch strings.ToUpper(lvl) {
	case "FATAL", "CRITICAL":
		return 4
	case "ERROR":
		return 3
	case "WARN", "WARNING":
		return 2
	default:
		return 1
	}
}

func (d *DrainMiner) treeSearch(root *DrainNode, tokens []string) *Cluster {
	tokenLen := len(tokens)
	lenKey := fmt.Sprintf("L%d", tokenLen)

	cur, ok := root.Children[lenKey]
	if !ok {
		return nil
	}

	// 按前缀最多导航 maxDepth 层
	depth := 1
	for depth < d.maxDepth && depth < tokenLen {
		token := tokens[depth-1]
		if next, exists := cur.Children[token]; exists {
			cur = next
		} else if wildcard, wExists := cur.Children["<*>"]; wExists {
			cur = wildcard
		} else {
			break
		}
		depth++
	}

	// 在叶节点的簇集合中计算序列相似度
	var bestCluster *Cluster
	maxSim := -1.0
	for _, c := range cur.Clusters {
		sim := seqSimilarity(c.Tokens, tokens)
		if sim >= d.simThreshold && sim > maxSim {
			maxSim = sim
			bestCluster = c
		}
	}
	return bestCluster
}

func (d *DrainMiner) treeAdd(root *DrainNode, c *Cluster) {
	tokens := c.Tokens
	tokenLen := len(tokens)
	lenKey := fmt.Sprintf("L%d", tokenLen)

	cur, ok := root.Children[lenKey]
	if !ok {
		cur = &DrainNode{Children: make(map[string]*DrainNode)}
		root.Children[lenKey] = cur
	}

	depth := 1
	for depth < d.maxDepth && depth < tokenLen {
		token := tokens[depth-1]
		if hasDigit(token) {
			token = "<*>"
		}
		next, exists := cur.Children[token]
		if !exists {
			next = &DrainNode{Children: make(map[string]*DrainNode)}
			cur.Children[token] = next
		}
		cur = next
		depth++
	}
	cur.Clusters = append(cur.Clusters, c)
}

func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func seqSimilarity(seq1, seq2 []string) float64 {
	if len(seq1) != len(seq2) {
		return 0.0
	}
	match := 0
	for i := 0; i < len(seq1); i++ {
		if seq1[i] == seq2[i] {
			match++
		}
	}
	return float64(match) / float64(len(seq1))
}

// GetTemplates 获取挖掘出的模板列表，按命中频次倒序排列
func (d *DrainMiner) GetTemplates() []model.LogTemplate {
	d.mu.Lock()
	defer d.mu.Unlock()

	res := make([]model.LogTemplate, 0, len(d.clusters))
	for _, c := range d.clusters {
		files := make([]string, 0, len(c.Files))
		for f := range c.Files {
			files = append(files, f)
		}
		sort.Strings(files)

		res = append(res, model.LogTemplate{
			ID:        c.ID,
			Pattern:   strings.Join(c.Tokens, " "),
			Sample:    c.Sample,
			Count:     c.Count,
			Level:     c.Level,
			FirstSeen: c.FirstSeen,
			LastSeen:  c.LastSeen,
			Files:     files,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		return res[i].Count > res[j].Count
	})

	return res
}

var mineBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 1024*1024)
		return &b
	},
}

// MineFile 流式分析单个日志文件，提取模板
func (d *DrainMiner) MineFile(filePath string, maxLines int) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	bufPtr := mineBufPool.Get().(*[]byte)
	defer mineBufPool.Put(bufPtr)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(*bufPtr, 10*1024*1024)

	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		d.AddLog(line, filePath)
		count++
		if maxLines > 0 && count >= maxLines {
			break
		}
	}
	return scanner.Err()
}

// ExtractTraceID 从任意日志行中自动提取分布式调用链路追踪 ID (TraceID/RequestID)
func ExtractTraceID(line string) string {
	if line == "" {
		return ""
	}
	if matches := reTraceID.FindStringSubmatch(line); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	// 针对纯 JSON 日志中的 "trace_id": "xxx" 或 "traceId": "xxx"
	idx := strings.Index(line, `"trace_id"`)
	if idx < 0 {
		idx = strings.Index(line, `"traceId"`)
	}
	if idx < 0 {
		idx = strings.Index(line, `"requestId"`)
	}
	if idx >= 0 {
		sub := line[idx:]
		colonIdx := strings.Index(sub, ":")
		if colonIdx >= 0 {
			valPart := strings.TrimSpace(sub[colonIdx+1:])
			if strings.HasPrefix(valPart, `"`) {
				valPart = valPart[1:]
				endQuote := strings.Index(valPart, `"`)
				if endQuote > 0 {
					return valPart[:endQuote]
				}
			}
		}
	}
	return ""
}
