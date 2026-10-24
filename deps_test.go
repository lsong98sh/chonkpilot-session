// 依赖方向断言（照 `src/lib/router` 的 TestDepsPureLib 口径）：`chonkpilot-session` 只允许
// **直接依赖** = 标准库 + 本模块自身包 + `chonkpilot-chat`。
//
// 2026-10-24（Persist Provider）：**`chonkpilot-data` 已从依赖图移除**（存储改由注入的
// `store.Store` 端口决定）；本测试与 TestDepsNoUpperLayers 同时兜住「data 不得再出现」。
//
// 任何 `chonkpilot-llm`（server）/ `chonkpilot-gui` / MQ 客户端 / gateway / router / data 进入
// 依赖图 → 本测试失败。
package session

import (
	"os/exec"
	"strings"
	"testing"
)

// depsAllowed 是直接依赖白名单（精确匹配）；本模块自身包按前缀放行。
var depsAllowed = []string{
	"github.com/lsong98sh/chonkpilot-chat",
}

// selfModule 是本模块前缀（组件内部包互相引用不算外部依赖）。
const selfModule = "github.com/chonkpilot/chonkpilot-session"

func TestDepsPureLib(t *testing.T) {
	out, err := exec.Command("go", "list", "-f",
		`{{.ImportPath}}|{{join .Imports " "}}|{{join .TestImports " "}}`, "./...").Output()
	if err != nil {
		t.Fatalf("go list 失败（需 go 工具链在 PATH 上）: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			t.Fatalf("go list 输出格式不符: %q", line)
		}
		pkg := parts[0]
		for _, field := range parts[1:] {
			for _, imp := range strings.Fields(field) {
				if imp == "" || isStdlib(imp) || strings.HasPrefix(imp, selfModule) || depsAllowedImport(imp) {
					continue
				}
				t.Fatalf("%s 依赖了白名单外的包 %q（只允许 stdlib + 本模块 + chonkpilot-chat）", pkg, imp)
			}
		}
	}
}

// depsAllowedImport 判定直接依赖是否在白名单内。
func depsAllowedImport(imp string) bool {
	for _, a := range depsAllowed {
		if a == imp {
			return true
		}
	}
	return false
}

// isStdlib 判定是否为标准库包（首段不含 "."）。
func isStdlib(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

// TestDepsNoUpperLayers 是**传递闭包**方向断言：上层实现（llm server / gui / router /
// gateway）与 MQ 客户端（mq / msgkeys）都不能出现在组件的依赖图里（哪怕间接）。
func TestDepsNoUpperLayers(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps 失败: %v", err)
	}
	forbidden := []string{
		"github.com/chonkpilot/chonkpilot-data",
		"github.com/chonkpilot/chonkpilot-llm",
		"github.com/chonkpilot/chonkpilot-gui",
		"github.com/chonkpilot/chonkpilot-lib/mq",
		"github.com/chonkpilot/chonkpilot-lib/msgkeys",
		"github.com/lsong98sh/chonkpilot-router",
		"github.com/lsong98sh/chonkpilot-mcp-gateway",
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		for _, f := range forbidden {
			if pkg == f || strings.HasPrefix(pkg, f+"/") {
				t.Fatalf("依赖图出现上层实现 %q（组件不得依赖它）", pkg)
			}
		}
	}
}
