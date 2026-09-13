package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"
)

const (
	defaultMaxChartPoints = 1500
	maxScriptOutputBytes  = 10 * 1024 * 1024 // 10MB
	scriptExecTimeout     = 30 * time.Second
)

// ChartEngine 时序点位脚本解析与降采样引擎
type ChartEngine struct{}

func NewChartEngine() *ChartEngine {
	return &ChartEngine{}
}

// ExecuteScript 执行客户自定义脚本并进行视口切片、极值降采样和突变识别
func (e *ChartEngine) ExecuteScript(
	ctx context.Context,
	rule *model.ChartScriptRule,
	targetFilePath string,
	startTime, endTime string,
	maxPoints int,
) (*model.ChartDataResponse, error) {
	if rule == nil {
		return nil, errors.New("chart script rule cannot be nil")
	}
	if maxPoints <= 0 {
		maxPoints = defaultMaxChartPoints
	}

	// 1. 检查目标日志文件是否存在
	fileInfo, err := os.Stat(targetFilePath)
	if err != nil {
		return nil, fmt.Errorf("target log file not accessible: %w", err)
	}
	if fileInfo.IsDir() {
		return nil, errors.New("target path is a directory, not a log file")
	}

	// 2. 准备临时脚本文件 (受限目录，只读或只执行)
	interpreter := strings.TrimSpace(rule.Interpreter)
	if interpreter == "" {
		interpreter = "/usr/bin/python3"
	}

	scriptContent := strings.TrimSpace(rule.ScriptContent)
	if scriptContent == "" {
		return nil, errors.New("script content is empty")
	}

	ext := ".py"
	if strings.Contains(interpreter, "bash") || strings.Contains(interpreter, "sh") {
		ext = ".sh"
	}
	tmpDir := os.TempDir()
	tmpScript := filepath.Join(tmpDir, fmt.Sprintf("chart_script_%s_%d%s", rule.ID, time.Now().UnixNano(), ext))
	if err := os.WriteFile(tmpScript, []byte(scriptContent), 0700); err != nil {
		return nil, fmt.Errorf("failed to write temporary script: %w", err)
	}
	defer os.Remove(tmpScript)

	// 3. 执行脚本 (带超时保护与输出上限)
	execCtx, cancel := context.WithTimeout(ctx, scriptExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, interpreter, tmpScript, targetFilePath)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("script execution timed out (>%v)", scriptExecTimeout)
		}
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, fmt.Errorf("script execution failed: %s", errMsg)
	}

	if stdoutBuf.Len() > maxScriptOutputBytes {
		return nil, fmt.Errorf("script output exceeded maximum limit (%d MB)", maxScriptOutputBytes/(1024*1024))
	}

	// 4. 解析输出 JSON
	var rawResp model.ChartDataResponse
	if err := json.Unmarshal(stdoutBuf.Bytes(), &rawResp); err != nil {
		// 若前缀有调试输出，尝试截取首个 '{' 到末尾 '}'
		rawBytes := stdoutBuf.Bytes()
		startIdx := bytes.IndexByte(rawBytes, '{')
		lastIdx := bytes.LastIndexByte(rawBytes, '}')
		if startIdx >= 0 && lastIdx > startIdx {
			if retryErr := json.Unmarshal(rawBytes[startIdx:lastIdx+1], &rawResp); retryErr == nil {
				err = nil
			}
		}
		if err != nil {
			snippet := string(stdoutBuf.Bytes())
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			return nil, fmt.Errorf("script output invalid JSON format: %s", snippet)
		}
	}

	// 补全基础字段
	if rawResp.Title == "" {
		rawResp.Title = rule.Name
	}
	rawResp.MatchedFile = filepath.Base(targetFilePath)
	rawResp.ScriptRuleID = rule.ID

	totalPoints := len(rawResp.XAxis.Data)
	rawResp.TotalPoints = totalPoints

	// 5. 局部时间窗口切片 (Zoom-in Slicing)
	if (startTime != "" || endTime != "") && totalPoints > 0 {
		rawResp = sliceByTimeWindow(rawResp, startTime, endTime)
	}

	// 6. 自动突变识别 (若脚本未主动提供，框架自动执行一阶差分与阈值检测)
	if len(rawResp.Anomalies) == 0 && len(rawResp.Series) > 0 && len(rawResp.XAxis.Data) > 0 {
		rawResp.Anomalies = detectAnomalies(rawResp.XAxis.Data, rawResp.Series)
	}

	// 7. 自适应极值降采样 (Min-Max Bucket Downsampling)
	currentPoints := len(rawResp.XAxis.Data)
	if currentPoints > maxPoints {
		rawResp = downsampleMinMax(rawResp, maxPoints)
		rawResp.Downsampled = true
		rawResp.Resolution = fmt.Sprintf("降采样全景 (保留瞬时极值尖峰, %d点)", len(rawResp.XAxis.Data))
	} else {
		rawResp.Downsampled = false
		if currentPoints > 0 {
			rawResp.Resolution = fmt.Sprintf("高精原始点位 (%d点)", currentPoints)
		} else {
			rawResp.Resolution = "无数据"
		}
	}

	if len(rawResp.XAxis.Data) > 0 {
		rawResp.StartTime = rawResp.XAxis.Data[0]
		rawResp.EndTime = rawResp.XAxis.Data[len(rawResp.XAxis.Data)-1]
	}

	return &rawResp, nil
}

// sliceByTimeWindow 根据前端传入的时间区间进行视口切片
func sliceByTimeWindow(resp model.ChartDataResponse, startTime, endTime string) model.ChartDataResponse {
	n := len(resp.XAxis.Data)
	if n == 0 {
		return resp
	}

	startIdx := 0
	endIdx := n - 1

	if startTime != "" {
		for i, t := range resp.XAxis.Data {
			if t >= startTime {
				startIdx = i
				break
			}
		}
	}
	if endTime != "" {
		for i := n - 1; i >= 0; i-- {
			if resp.XAxis.Data[i] <= endTime {
				endIdx = i
				break
			}
		}
	}

	if startIdx > endIdx {
		return resp
	}

	// 切片横坐标
	slicedX := resp.XAxis.Data[startIdx : endIdx+1]

	// 切片指标系列
	slicedSeries := make([]model.ChartSeries, len(resp.Series))
	for sIdx, s := range resp.Series {
		slicedSeries[sIdx] = model.ChartSeries{
			Name:      s.Name,
			Unit:      s.Unit,
			ChartType: s.ChartType,
		}
		if len(s.Data) >= endIdx+1 {
			slicedSeries[sIdx].Data = s.Data[startIdx : endIdx+1]
		} else if len(s.Data) > startIdx {
			slicedSeries[sIdx].Data = s.Data[startIdx:]
		}
	}

	// 重新映射与过滤突变列表
	var slicedAnomalies []model.ChartAnomaly
	for _, a := range resp.Anomalies {
		if a.Index >= startIdx && a.Index <= endIdx {
			a.Index -= startIdx
			slicedAnomalies = append(slicedAnomalies, a)
		}
	}

	resp.XAxis.Data = slicedX
	resp.Series = slicedSeries
	resp.Anomalies = slicedAnomalies
	return resp
}

// downsampleMinMax 采用 Min-Max 分桶保留极值算法进行降采样，确保波峰（100% 尖峰）与波谷绝对不丢失
func downsampleMinMax(resp model.ChartDataResponse, targetPoints int) model.ChartDataResponse {
	n := len(resp.XAxis.Data)
	if n <= targetPoints || targetPoints < 4 {
		return resp
	}

	// 每个桶取 2 个点（Min 与 Max）
	numBuckets := targetPoints / 2
	bucketSize := float64(n) / float64(numBuckets)

	// 用于记录被选中的原始索引，并去重保持有序
	selectedIndicesMap := make(map[int]struct{})
	// 首尾点必须保留
	selectedIndicesMap[0] = struct{}{}
	selectedIndicesMap[n-1] = struct{}{}

	// 同时必须保留所有已识别出的突变异常点
	for _, a := range resp.Anomalies {
		if a.Index >= 0 && a.Index < n {
			selectedIndicesMap[a.Index] = struct{}{}
		}
	}

	// 遍历所有指标系列，分桶寻找最大与最小点所在索引
	for _, s := range resp.Series {
		if len(s.Data) != n {
			continue
		}
		for b := 0; b < numBuckets; b++ {
			bStart := int(math.Floor(float64(b) * bucketSize))
			bEnd := int(math.Floor(float64(b+1) * bucketSize))
			if bEnd > n {
				bEnd = n
			}
			if bStart >= bEnd {
				continue
			}

			minVal := math.MaxFloat64
			maxVal := -math.MaxFloat64
			minIdx := bStart
			maxIdx := bStart

			for i := bStart; i < bEnd; i++ {
				v := s.Data[i]
				if v < minVal {
					minVal = v
					minIdx = i
				}
				if v > maxVal {
					maxVal = v
					maxIdx = i
				}
			}

			selectedIndicesMap[minIdx] = struct{}{}
			selectedIndicesMap[maxIdx] = struct{}{}
		}
	}

	// 排序索引
	sortedIndices := make([]int, 0, len(selectedIndicesMap))
	for idx := range selectedIndicesMap {
		sortedIndices = append(sortedIndices, idx)
	}
	sort.Ints(sortedIndices)

	// 构造新的 X 轴
	newX := make([]string, len(sortedIndices))
	for i, origIdx := range sortedIndices {
		newX[i] = resp.XAxis.Data[origIdx]
	}

	// 构造新的各 Series 数据
	newSeries := make([]model.ChartSeries, len(resp.Series))
	for sIdx, s := range resp.Series {
		newSeries[sIdx] = model.ChartSeries{
			Name:      s.Name,
			Unit:      s.Unit,
			ChartType: s.ChartType,
			Data:      make([]float64, len(sortedIndices)),
		}
		for i, origIdx := range sortedIndices {
			if origIdx < len(s.Data) {
				newSeries[sIdx].Data[i] = s.Data[origIdx]
			}
		}
	}

	// 映射突变点的新索引
	origToNewIndex := make(map[int]int)
	for newIdx, origIdx := range sortedIndices {
		origToNewIndex[origIdx] = newIdx
	}

	var newAnomalies []model.ChartAnomaly
	for _, a := range resp.Anomalies {
		if newIdx, ok := origToNewIndex[a.Index]; ok {
			a.Index = newIdx
			newAnomalies = append(newAnomalies, a)
		}
	}

	resp.XAxis.Data = newX
	resp.Series = newSeries
	resp.Anomalies = newAnomalies
	return resp
}

// detectAnomalies 框架通用突变识别：一阶差分斜率跃变 (|Δy| > 3σ) + 高水位阈值突破
func detectAnomalies(xAxis []string, series []model.ChartSeries) []model.ChartAnomaly {
	var anomalies []model.ChartAnomaly
	n := len(xAxis)
	if n < 3 {
		return anomalies
	}

	for _, s := range series {
		if len(s.Data) != n {
			continue
		}

		// 1. 计算均值与一阶差分
		diffs := make([]float64, n-1)
		var diffSum float64
		for i := 1; i < n; i++ {
			diff := math.Abs(s.Data[i] - s.Data[i-1])
			diffs[i-1] = diff
			diffSum += diff
		}
		meanDiff := diffSum / float64(n-1)

		var varDiffSum float64
		for _, d := range diffs {
			varDiffSum += (d - meanDiff) * (d - meanDiff)
		}
		stdDiff := math.Sqrt(varDiffSum / float64(n-1))
		if stdDiff < 1e-6 {
			stdDiff = 1.0
		}

		// 2. 判定跃变
		for i := 1; i < n; i++ {
			diff := diffs[i-1]
			val := s.Data[i]
			prev := s.Data[i-1]

			isSpike := diff > (meanDiff + 3.0*stdDiff) && diff > 10.0
			isCriticalValue := val >= 90.0 && s.Unit == "%"

			if isSpike || isCriticalValue {
				reason := fmt.Sprintf("%s 突增至 %.1f%s (前值: %.1f%s, 波动: +%.1f)", s.Name, val, s.Unit, prev, s.Unit, diff)
				if val < prev {
					reason = fmt.Sprintf("%s 骤降至 %.1f%s (前值: %.1f%s, 跌落: -%.1f)", s.Name, val, s.Unit, prev, s.Unit, diff)
				}
				sev := "WARNING"
				if val >= 95.0 || diff >= 50.0 {
					sev = "CRITICAL"
				}

				anomalies = append(anomalies, model.ChartAnomaly{
					Time:     xAxis[i],
					Index:    i,
					Metric:   s.Name,
					Value:    val,
					Severity: sev,
					Reason:   reason,
				})

				// 避免紧挨着的连续报警
				if len(anomalies) >= 15 {
					break
				}
			}
		}
	}

	// 按时间升序排序
	sort.Slice(anomalies, func(i, j int) bool {
		return anomalies[i].Index < anomalies[j].Index
	})

	return anomalies
}
