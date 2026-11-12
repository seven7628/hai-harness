// memory_dream：离线记忆盘点（P4 v1，**只报不改**）。
//
// 用法：
//
//	go run ./memory/cmd/memory_dream                      # 盘点全部仓库
//	go run ./memory/cmd/memory_dream --repo <repo-id>     # 只看一个
//	go run ./memory/cmd/memory_dream --root <dir>         # 换记忆根（默认 ~/.go-code/memory/repos）
//
// 为什么单独一个命令行工具而不是接进 GUI：v1 的目的是"让人先看几轮报告，再决定
// 哪一类可以自动化"。做成 GUI 按钮等于提前承诺它会定期跑、结果会被当真；而一份
// 没人看的报告比没有报告更糟。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

func main() {
	var (
		root  = flag.String("root", "", "记忆根目录（默认 ~/.go-code/memory/repos）")
		repo  = flag.String("repo", "", "只盘点这个 repo-id")
		bind  = flag.String("bind", "", "把这个工作区与它的记忆目录绑定（写 .state/repo.json），然后退出")
		evid  = flag.String("evidence", "", "把每条记忆的证据导出成 JSON（交给模型裁决）")
		apply = flag.String("apply", "", "执行模型裁决 JSON：archive / keep（带快照与日志）")
		conf  = flag.String("conflicts", "", "导出待判冲突候选 JSON（交给模型判断有没有被推翻）")
		savec = flag.String("save-conflicts", "", "保存模型给出的冲突判定 JSON")
		rest  = flag.String("restore", "", "把一个快照里的归档原样恢复（文件 + 索引行）")
		prot  = flag.String("protect", "", "从评测语料生成保护清单（这些条目不许归档）")
	)
	flag.Parse()

	// 绑定只需要解析一次身份，与盘点无关，所以先处理并直接返回。
	if *bind != "" {
		d, wss, err := memory.Config{}.BindRepoDiscovered(context.Background(), *bind)
		if err != nil {
			fail("绑定失败：%v", err)
		}
		fmt.Printf("已绑定：%s → %s\n", d.Path, *bind)
		fmt.Printf("发现工作区 %d 个：\n", len(wss))
		for _, w := range wss {
			fmt.Println("  " + w)
		}
		return
	}

	base := *root
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fail("取不到 home 目录：%v", err)
		}
		base = filepath.Join(home, ".go-code", "memory", "repos")
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		fail("读记忆根失败：%v", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && (*repo == "" || e.Name() == *repo) {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		fail("在 %s 下没有找到匹配的记忆仓库", base)
	}
	sort.Strings(ids)

	now := time.Now()
	cfg := memory.Config{}
	if *evid != "" || *apply != "" {
		ids = filterByRepo(ids, *repo)
	}

	for _, id := range ids {
		d := memory.Dir{ID: id, Path: filepath.Join(base, id)}
		if *prot != "" {
			raw, err := os.ReadFile(*prot)
			if err != nil {
				fail("读评测语料失败：%v", err)
			}
			var corpus struct {
				Items []struct {
					Repo string `json:"repo"`
					File string `json:"file"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &corpus); err != nil {
				fail("解析评测语料失败：%v", err)
			}
			var files []string
			for _, it := range corpus.Items {
				if it.Repo != id {
					continue
				}
				name := it.File
				if !strings.HasSuffix(name, ".md") {
					name += ".md"
				}
				files = append(files, name)
			}
			if err := cfg.WriteProtected(d, files); err != nil {
				fail("写保护清单失败：%v", err)
			}
			fmt.Printf("%s：保护 %d 条\n", id, len(files))
			continue
		}
		if *rest != "" {
			got, err := cfg.Restore(d, *rest)
			if err != nil {
				fail("%s 恢复失败：%v", id, err)
			}
			fmt.Printf("%s：恢复 %d 条\n", id, len(got))
			for _, f := range got {
				fmt.Println("  恢复 " + f)
			}
			continue
		}
		if *conf != "" {
			cands, err := cfg.ConflictCandidates(d, 180*24*time.Hour)
			if err != nil {
				fail("%s 取冲突候选失败：%v", id, err)
			}
			out, _ := json.MarshalIndent(map[string]any{"repo": id, "candidates": cands}, "", "  ")
			path := strings.TrimSuffix(*conf, ".json") + "-" + id + ".json"
			if err := os.WriteFile(path, out, 0o600); err != nil {
				fail("写冲突候选失败：%v", err)
			}
			fmt.Printf("%s：候选 %d 条 → %s\n", id, len(cands), path)
			continue
		}
		if *savec != "" {
			raw, err := os.ReadFile(*savec)
			if err != nil {
				fail("读判定失败：%v", err)
			}
			var payload struct {
				Verdicts []memory.ConflictVerdict `json:"verdicts"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				fail("解析判定失败：%v", err)
			}
			if err := cfg.SaveConflicts(d, payload.Verdicts); err != nil {
				fail("保存判定失败：%v", err)
			}
			fmt.Printf("%s：已保存 %d 条判定\n", id, len(payload.Verdicts))
			continue
		}
		if *apply != "" {
			raw, err := os.ReadFile(*apply)
			if err != nil {
				fail("读裁决文件失败：%v", err)
			}
			decs, err := memory.ParseDecisions(raw)
			if err != nil {
				fail("解析裁决失败：%v", err)
			}
			res, err := cfg.ApplyDecisions(d, decs)
			if err != nil {
				fail("%s 应用失败：%v", id, err)
			}
			fmt.Printf("%s：归档 %d 条，跳过 %d 条（快照 %s）\n", id, len(res.Archived), len(res.Skipped), res.Snapshot)
			for _, f := range res.Archived {
				fmt.Println("  归档 " + f)
			}
			for _, f := range res.Skipped {
				fmt.Println("  跳过 " + f)
			}
			return
		}
		if *evid != "" {
			ev, err := cfg.Evidence(d)
			if err != nil {
				fail("%s 取证失败：%v", id, err)
			}
			out, _ := json.MarshalIndent(ev, "", "  ")
			path := *evid
			if len(ids) > 1 {
				path = strings.TrimSuffix(*evid, ".json") + "-" + id + ".json"
			}
			if err := os.WriteFile(path, out, 0o600); err != nil {
				fail("写证据文件失败：%v", err)
			}
			fmt.Printf("%s：证据 → %s\n", id, path)
			continue
		}
	}
	for i, id := range ids {
		if i > 0 {
			fmt.Println("\n---\n")
		}
		rep, err := cfg.Dream(memory.Dir{ID: id, Path: filepath.Join(base, id)})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[dream] %s 读取失败：%v\n", id, err)
			continue
		}
		fmt.Print(rep.Markdown(now))
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

// filterByRepo 在 --repo 指定时只保留那一个（其它路径已经由 ids 过滤过）。
func filterByRepo(ids []string, repo string) []string {
	if repo == "" {
		return ids
	}
	for _, id := range ids {
		if id == repo {
			return []string{id}
		}
	}
	return ids
}
