// preview_cancels.go —— 在飞预览转换的取消表（R08a 纪律面）。
//
// 为什么需要它：一次转换最长 60s、并发只有 2。工作区被关闭（delete_workspace /
// 应用退出 / 切工作区重建）之后还在跑的转换没有任何意义 —— 它占着一个槽位、握着一份
// 沙箱、最后还可能往缓存里写一份用户不会再看的产物。取消沿着 ctx 一路传到
// sandbox.runExec 的 kill(-pgid)，转换子进程一并收掉（不是「不管它，等它自己结束」）。
//
// 为什么**不**在「同一个文件再次请求」时取消上一次（评审的另一个候选取舍点）：
// 同一个文件有两个**都合法**的并发消费者 —— 预览面板（loadOfficePreview，把 HTML
// 读回前端）与原生视图（previewOfficeFile，开浏览器标签）。它们各自发一条 xlsx_preview。
// 按目标取消会让它们互相作废，用户看到的是「随机一个入口报错」。前端已经按 path 去重了
// 重复点击（officeBusy），后端再猜一次只会帮倒忙。故取消的唯一触发点是**生命周期**
// （工作区关闭/退出），不是请求压力；请求压力由 queue_timeout 兜。
package main

import (
	"context"
	"path/filepath"
	"sync"
)

// previewCancels 在飞转换的取消表。零值可用（map 懒建）。
type previewCancels struct {
	mu   sync.Mutex
	seq  int
	live map[int]previewCancelEntry
}

type previewCancelEntry struct {
	ws     string
	cancel context.CancelFunc
}

// begin 登记一次在飞转换：返回可取消的 ctx 与幂等的收尾函数。
// 收尾函数必须在转换 goroutine 结束时调用（defer），否则表会泄漏。
func (p *previewCancels) begin(ws string) (context.Context, func()) {
	// 归一化：shutdownAll 传的是 registry 里的归一化 key，命令侧传的是 payload 原串
	// （尾斜杠、软链接、`..` 都可能不同）。不 Clean 就会「取消不掉」—— 而取消表
	// 正是拿来兜「工作区已经关了」的，漏取消等于白表。
	ws = filepath.Clean(ws)
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	if p.live == nil {
		p.live = map[int]previewCancelEntry{}
	}
	p.seq++
	id := p.seq
	p.live[id] = previewCancelEntry{ws: ws, cancel: cancel}
	p.mu.Unlock()
	return ctx, func() {
		p.mu.Lock()
		delete(p.live, id)
		p.mu.Unlock()
		cancel() // 幂等；即使已被 cancelWorkspace 取消过也无害
	}
}

// cancelWorkspace 取消某工作区的全部在飞转换（工作区关闭/退出时调用）。
func (p *previewCancels) cancelWorkspace(ws string) {
	ws = filepath.Clean(ws)
	p.mu.Lock()
	var victims []context.CancelFunc
	for id, e := range p.live {
		if e.ws == ws {
			victims = append(victims, e.cancel)
			delete(p.live, id)
		}
	}
	p.mu.Unlock()
	// 锁外取消：cancel 会同步跑 ctx 的监听者（sandbox 那边是 kill 进程组），
	// 不该占着这张表的锁。
	for _, c := range victims {
		c()
	}
}

// count 当前在飞数量（测试/诊断用）。
func (p *previewCancels) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.live)
}
