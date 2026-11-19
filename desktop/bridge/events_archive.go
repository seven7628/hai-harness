package main

// 事件日志的**后台 gzip 封存**（2026-11-19，爬 4）。
//
// ## 为什么
//
// 事件日志（`~/.go-code/events/<wsKey>/<sid>.jsonl`）是**只增不减**的：
// 实测 528 个文件 / **6.7GB**，最早到 2026-08-29，**全仓库没有任何保留期逻辑**
// ⇒ 一根只涨不消的柱子。
//
// 内容是 **JSON 文本** ⇒ gzip 压缩比 6–7×（实测）⇒ 6.7GB → 约 1GB，
// **内容一点不丢**（用户明确要求：events 不要删，要支持 zip 压缩）。
//
// ## 形态
//
//	活动会话：`<sid>.jsonl`（明文，还在追加）
//	封存会话：`<sid>.jsonl.gz`（压缩后只读）
//
// 读侧透明（`internal/eventslog.OpenReader`：有明文读明文，没有读 .gz）；
// 写侧（`eventslog.Restore`）：会话被重新激活时先解压还原再追加。
//
// ## 判据：mtime 够老
//
// 只封存 **mtime 超过 minAge** 的文件。
//
// 为什么不需要额外的「会话是否活跃」判据：
//  · 正在被写入的文件 mtime 必然是「刚刚」⇒ 天然被排除；
//  · 30 天没动但今天被打开的会话 ⇒ 两条防线兜住：
//    ① 压缩期间被追加 ⇒ mtime/size 变了 ⇒ `eventslog.Archive` **放弃封存**；
//    ② 封存完成后才被追加 ⇒ 写侧 `Restore` 先解压还原再写。
//
// ## 每轮预算
//
// `maxBytes`：一轮最多压多少字节 ⇒ 封存是**后台限速**跑的，不能把启动期的
// IO/CPU 吃满（实测 6.7GB 全压约 1–2 分钟 CPU）。
// 没压完的下一轮继续（按 mtime 排序 ⇒ 先压最老的）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/eventslog"
)

// eventsArchiveMinAge 是「值得封存」的最小年龄（默认 30 天）。
//
// 为什么是 30 天：足够老 ⇒ 大概率不会被重新激活（省掉「解压还原」的代价）；
// 又足够新 ⇒ 用户的「最近会话」列表不受影响。
const eventsArchiveMinAge = 30 * 24 * time.Hour

// eventsArchiveMaxBytesPerRun 是单轮封存的字节预算（默认 2GB）。
const eventsArchiveMaxBytesPerRun = 2 << 30

// eventsArchiveResult 是封存的结果。
type eventsArchiveResult struct {
	Files    int   // 封存成功的文件数
	Skipped  int   // 太新 / 已有 .gz 而跳过
	Aborted  int   // 压缩期间被改动而放弃
	Before   int64 // 封存前总字节
	After    int64 // 封存后总字节
	Elapsed  time.Duration
	ReachedB bool // 是否撞到单轮预算（还有没压完的）
}

// Saved 返回省下的字节数。
func (r eventsArchiveResult) Saved() int64 { return r.Before - r.After }

// archiveEventsFiles 把久未活动的事件日志压成 .gz。
//
// 参数：
//   - eventsRoot：事件日志根目录（~/.go-code/events）
//   - minAge：最小年龄（比它新的不碰）
//   - maxBytes：单轮字节预算（0 = 不限）
//   - pause：每个文件之间的间隔（限速）
func archiveEventsFiles(eventsRoot string, minAge time.Duration, maxBytes int64, pause time.Duration) (eventsArchiveResult, error) {
	res := eventsArchiveResult{}
	start := time.Now()
	if eventsRoot == "" {
		return res, fmt.Errorf("events archive: empty root")
	}
	cutoff := time.Now().Add(-minAge)

	// 收集候选（按 mtime 升序 ⇒ 先压最老的）
	type cand struct {
		path  string
		mtime time.Time
		size  int64
	}
	var cands []cand
	wsDirs, err := os.ReadDir(eventsRoot)
	if err != nil {
		return res, err
	}
	for _, ws := range wsDirs {
		if !ws.IsDir() {
			continue
		}
		dir := filepath.Join(eventsRoot, ws.Name())
		files, ferr := os.ReadDir(dir)
		if ferr != nil {
			continue
		}
		for _, fe := range files {
			if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, fe.Name())
			// 已经有 .gz ⇒ 明文是更新的那份（有人在写）⇒ 不碰。
			if _, gerr := os.Stat(path + eventslog.GzSuffix); gerr == nil {
				res.Skipped++
				continue
			}
			fi, serr := os.Stat(path)
			if serr != nil {
				continue
			}
			if fi.ModTime().After(cutoff) {
				res.Skipped++
				continue
			}
			cands = append(cands, cand{path: path, mtime: fi.ModTime(), size: fi.Size()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime.Before(cands[j].mtime) })

	var budget int64
	for _, c := range cands {
		if maxBytes > 0 && budget+c.size > maxBytes {
			res.ReachedB = true
			break
		}
		before := c.size
		ok, aerr := eventslog.Archive(c.path, eventslog.DefaultLevel)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "[events] 封存失败 %s: %v\n", c.path, aerr)
			continue
		}
		if !ok {
			res.Aborted++
			continue
		}
		res.Files++
		res.Before += before
		if fi, serr := os.Stat(c.path + eventslog.GzSuffix); serr == nil {
			res.After += fi.Size()
		}
		budget += before
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	res.Elapsed = time.Since(start)
	return res, nil
}
