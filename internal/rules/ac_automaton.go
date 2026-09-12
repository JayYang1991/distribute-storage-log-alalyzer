package rules

// acNode AC 自动机 Trie 树节点
type acNode struct {
	children map[byte]*acNode
	fail     *acNode
	ruleIDs  []int // 命中时触发的规则索引列表
}

func newACNode() *acNode {
	return &acNode{
		children: make(map[byte]*acNode),
	}
}

// ACAutomaton 纯 Go 高性能 Aho-Corasick 多模式字符串匹配机
type ACAutomaton struct {
	root *acNode
}

// NewACAutomaton 根据给定的关键词和规则映射列表构建 AC 自动机
func NewACAutomaton(keywords [][]byte, ruleIndices []int) *ACAutomaton {
	root := newACNode()
	ac := &ACAutomaton{root: root}

	// 1. 构建 Trie 树
	for idx, kw := range keywords {
		if len(kw) == 0 {
			continue
		}
		ruleIdx := ruleIndices[idx]
		curr := root
		for _, b := range kw {
			next, ok := curr.children[b]
			if !ok {
				next = newACNode()
				curr.children[b] = next
			}
			curr = next
		}
		curr.ruleIDs = append(curr.ruleIDs, ruleIdx)
	}

	// 2. BFS 构建 Fail 失败指针与输出链接
	var queue []*acNode
	for _, child := range root.children {
		child.fail = root
		queue = append(queue, child)
	}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for b, child := range curr.children {
			// 寻找 child 的 fail 节点
			failNode := curr.fail
			for failNode != nil {
				if next, ok := failNode.children[b]; ok {
					child.fail = next
					break
				}
				failNode = failNode.fail
			}
			if failNode == nil {
				child.fail = root
			}

			// 合并 fail 节点的匹配项
			if len(child.fail.ruleIDs) > 0 {
				child.ruleIDs = append(child.ruleIDs, child.fail.ruleIDs...)
			}

			queue = append(queue, child)
		}
	}

	return ac
}

// MatchOneLine 在单行小写字节序列中一次性找出命中的所有规则索引 (时间复杂度 O(N))
// matchedRuleBitset 采用布尔切片去重，避免高频堆分配
func (ac *ACAutomaton) MatchOneLine(line []byte, hitSet []bool) []int {
	if ac == nil || ac.root == nil || len(line) == 0 {
		return nil
	}

	var matchedIndices []int
	curr := ac.root

	for _, b := range line {
		for curr != nil {
			if next, ok := curr.children[b]; ok {
				curr = next
				break
			}
			curr = curr.fail
		}
		if curr == nil {
			curr = ac.root
		}

		if len(curr.ruleIDs) > 0 {
			for _, rIdx := range curr.ruleIDs {
				if rIdx >= 0 && rIdx < len(hitSet) {
					if !hitSet[rIdx] {
						hitSet[rIdx] = true
						matchedIndices = append(matchedIndices, rIdx)
					}
				}
			}
		}
	}

	return matchedIndices
}
