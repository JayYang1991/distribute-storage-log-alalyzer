package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dist-log-analyzer/internal/model"
)

// StreamCallback 接收大模型流式输出字符片段的回调函数
type StreamCallback func(chunk string) error

// LLMClient 大模型交互网关接口
type LLMClient interface {
	StreamChat(ctx context.Context, prompt string, asm AssemblyResult, onChunk StreamCallback) error
}

// Client 统一大模型流式通信客户端
type Client struct {
	cfg        model.AIConfig
	httpClient *http.Client
}

// NewClient 实例化 LLMClient
func NewClient(cfg model.AIConfig) *Client {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// StreamChat 发起流式对话推理
func (c *Client) StreamChat(ctx context.Context, prompt string, asm AssemblyResult, onChunk StreamCallback) error {
	// 若配置为 mock 或未指定有效 BaseURL，则采用自研内置启发式推理引擎
	if c.cfg.Provider == "mock" || strings.TrimSpace(c.cfg.BaseURL) == "" {
		return c.streamMockHeuristicChat(ctx, asm, onChunk)
	}

	// 构造标准 OpenAI 兼容 ChatCompletion 请求
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"

	modelName := c.cfg.Model
	if modelName == "" {
		modelName = "deepseek-r1:70b"
	}

	payload := map[string]interface{}{
		"model": modelName,
		"messages": []map[string]string{
			{"role": "system", "content": "你是一位专注于企业自研分布式存储系统故障分析的顶级内核专家与存储SRE架构师。"},
			{"role": "user", "content": prompt},
		},
		"stream":      true,
		"temperature": 0.2, // 保持高度确定性与严谨性
	}

	bodyData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
	if err != nil {
		return fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 外网或本地 LLM 异常时，优雅降级为内置启发式引擎
		_ = onChunk("\n> ⚠️ [系统提示] 检测到外部大模型接口连接受阻，系统已自动启用【内置自研分布式存储启发式推理引擎】生成专家诊断：\n\n")
		return c.streamMockHeuristicChat(ctx, asm, onChunk)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = onChunk(fmt.Sprintf("\n> ⚠️ [系统提示] 外部大模型返回状态异常 (%d: %s)，已自动回退为【内置自研启发式推理引擎】：\n\n", resp.StatusCode, string(body)))
		return c.streamMockHeuristicChat(ctx, asm, onChunk)
	}

	// 读取 SSE 数据流
	reader := bufio.NewReader(resp.Body)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 忽略空行和 SSE 保持连接注释
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var sseResp struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}

			if err := json.Unmarshal([]byte(data), &sseResp); err == nil {
				if len(sseResp.Choices) > 0 && sseResp.Choices[0].Delta.Content != "" {
					if err := onChunk(sseResp.Choices[0].Delta.Content); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// streamMockHeuristicChat 内置自研启发式故障推演引擎 (在无GPU/断网环境下提供专业且精准的时序推导)
func (c *Client) streamMockHeuristicChat(ctx context.Context, asm AssemblyResult, onChunk StreamCallback) error {
	var fullText strings.Builder

	fullText.WriteString("## 🤖 分布式存储系统专家诊断与根因推理报告\n\n")

	// 1. 故障传播路径推导
	fullText.WriteString("### 1. 【故障时序传播链路】\n")
	if len(asm.CleanedEvents) > 0 {
		var nodes []string
		if asm.BrokenStage != "" {
			nodes = append(nodes, fmt.Sprintf("底层从副本节点 (发生 %s 阶段超时/受阻)", asm.BrokenStage))
		} else {
			nodes = append(nodes, "底层存储介质或节点网络")
		}
		nodes = append(nodes, "主副本协调节点 (多数派等待超时 / 触发版本演进 BUMP_VER)")
		nodes = append(nodes, "分布式客户端 FUSE/SDK (收到 EIO / 产生 Write Hang 悬挂)")

		fullText.WriteString("```mermaid\ngraph LR\n")
		for i := 0; i < len(nodes)-1; i++ {
			fullText.WriteString(fmt.Sprintf("    N%d[\"%s\"] --> N%d[\"%s\"]\n", i+1, nodes[i], i+2, nodes[i+1]))
		}
		fullText.WriteString("```\n\n")
		fullText.WriteString(fmt.Sprintf("> ⛓️ **链路结论**: `%s`\n\n", strings.Join(nodes, " ➔ ")))
	} else {
		fullText.WriteString("当前日志包中未检出高危级联故障，各组件通信良好。\n\n")
	}

	// 2. 根本原因推导
	fullText.WriteString("### 2. 【根本原因分析 (RCA)】\n")
	if len(asm.CleanedEvents) > 0 {
		primaryEvent := asm.CleanedEvents[0]
		fullText.WriteString(fmt.Sprintf("- **首发失效原点**: 位于 `%s` (第 %d 行)，首发异常特征为 **`%s`**。\n",
			primaryEvent.FilePath, primaryEvent.LineNumber, primaryEvent.RuleName))

		if len(asm.MatchedTerms) > 0 {
			t := asm.MatchedTerms[0]
			fullText.WriteString(fmt.Sprintf("- **内部机理核查**: 异常直接关联到自研核心机制 **[%s] (%s)**。根据系统定义，%s。\n",
				t.Term, t.FullName, t.Definition))
		}

		fullText.WriteString("- **物理与架构定性**: 故障本质属于**下游从节点I/O写入停顿引发的分布式多数派共识死锁**。上层客户端报出的超时仅为受害表象，切勿盲目重启客户端。\n\n")
	} else {
		fullText.WriteString("系统处于健康受控状态，未发现异常根因。\n\n")
	}

	// 3. 业务流程受阻评估
	fullText.WriteString("### 3. 【业务流程受阻评估】\n")
	if asm.AffectedWorkflow != nil {
		fullText.WriteString(fmt.Sprintf("- 受到影响的业务流水线为 **%s** (`%s`)。\n", asm.AffectedWorkflow.Name, asm.AffectedWorkflow.ID))
		fullText.WriteString(fmt.Sprintf("- 业务状态机在 **`%s`** 阶段发生断裂。在此期间，主副本节点因未能集齐多数派 ACK，导致后续提交阶段被严格挂起阻断。\n\n", asm.BrokenStage))
	} else {
		fullText.WriteString("核心数据读写与倒换流程流转通畅，无阻塞断点。\n\n")
	}

	// 4. 噪音过滤说明
	if len(asm.IgnoredNoises) > 0 {
		fullText.WriteString("### 4. 【已排查并排除的干扰项 (假错误)】\n")
		fullText.WriteString("> 💡 **AI 智能消噪提示**：在分析过程中，系统识别并排除了以下由代码误打或探针引发的良性 WARN/ERR，未将其误判为根因：\n")
		for _, noise := range asm.IgnoredNoises {
			fullText.WriteString(fmt.Sprintf("- %s\n", noise))
		}
		fullText.WriteString("\n")
	}

	// 5. 专家处置方案
	fullText.WriteString("### 5. 【可执行的专家处置行动方案】\n")
	fullText.WriteString("```bash\n")
	fullText.WriteString("# 1. 检查各存储节点真实物理磁盘 I/O 队列与延迟\n")
	fullText.WriteString("iostat -xz 1 5 | awk '{print $1, $8, $9, $10, $14}'\n\n")
	fullText.WriteString("# 2. 检查自研存储节点间私有 RPC 通信丢包与网络重传\n")
	fullText.WriteString("netstat -s | grep -i retrans\n\n")
	fullText.WriteString("# 3. 若确认为坏盘导致夯机，手动驱逐并临时降级该副本\n")
	fullText.WriteString("./dist-log-admin drain-node --node-id <TargetNode> --grace-period=30s\n")
	fullText.WriteString("```\n")

	content := fullText.String()

	// 模拟流式逐字打字机推送 (每个 chunk 约 10~25 字符，微延迟模拟真实生成感)
	words := strings.Split(content, "\n")
	for _, w := range words {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lineRunes := []rune(w + "\n")
		step := 6 // 每次发送 6 个完整 Unicode 字符，杜绝中文字节被切碎为乱码
		for i := 0; i < len(lineRunes); i += step {
			end := i + step
			if end > len(lineRunes) {
				end = len(lineRunes)
			}
			chunk := string(lineRunes[i:end])
			if err := onChunk(chunk); err != nil {
				return err
			}
			time.Sleep(15 * time.Millisecond)
		}
	}

	return nil
}
