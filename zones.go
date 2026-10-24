// zones.go —— 三段上下文边界与预存 token 对齐（纯函数；压缩侧与组装侧共用的唯一实现）。
//
// 2026-10-24（Persist Provider 改造）：自 `chonkpilot-data` 内化到本组件，算法逐字不变。
package session

import "github.com/chonkpilot/chonkpilot-session/store"

// LocateFullTurnCount 计算「保留完整」的轮数（口径 V/W，见 41 I-21）：
//
//	本轮恒保留完整（下限 1）；从更近轮向前逐轮纳入，直到达到 maxTurns 或累计超 maxTokens；
//	maxTurns/maxTokens <= 0 → 该条件不启用；两者均 <= 0 → 不压缩（返回 len）。
func LocateFullTurnCount(fullTokens []int, maxTurns, maxTokens int) int {
	n := len(fullTokens)
	if n == 0 {
		return 0
	}
	if maxTurns <= 0 && maxTokens <= 0 {
		return n
	}
	included := 1
	cum := fullTokens[n-1]
	for i := 1; i < n; i++ {
		if maxTurns > 0 && included >= maxTurns {
			break
		}
		cum += fullTokens[n-1-i]
		if maxTokens > 0 && cum > maxTokens {
			break
		}
		included++
	}
	return included
}

// ResolveStoredTokens 用预存 token 值覆盖实时估算值（尾部对齐；缺值回退，绝不把缺值当 0）。
func ResolveStoredTokens(stored, estimated []int) []int {
	out := make([]int, len(estimated))
	copy(out, estimated)
	off := len(estimated) - len(stored)
	start := 0
	if off < 0 {
		start = -off
		off = 0
	}
	for i, v := range stored[start:] {
		if v <= 0 {
			continue
		}
		if j := off + i; j >= 0 && j < len(out) {
			out[j] = v
		}
	}
	return out
}

// LocateZones 计算三段边界（完整区复用 LocateFullTurnCount；简化区自完整区前一轮向前累加
// briefTokens，累计 <= briefBudget 即纳入；briefBudget <= 0 → 简化区 0 轮）。
func LocateZones(fullTokens, briefTokens []int, maxTurns, maxTokens, briefBudget int) store.Zones {
	n := len(fullTokens)
	if n == 0 {
		return store.Zones{}
	}
	full := LocateFullTurnCount(fullTokens, maxTurns, maxTokens)
	fullStart := n - full
	brief := 0
	if briefBudget > 0 && fullStart > 0 && fullStart <= len(briefTokens) {
		cum := 0
		for i := fullStart - 1; i >= 0; i-- {
			if cum+briefTokens[i] > briefBudget {
				break
			}
			cum += briefTokens[i]
			brief++
		}
	}
	return store.Zones{FullTurns: full, BriefTurns: brief}
}
