package main

// 预览缓存的**运行期**周期清扫（2026-11-18「资源有界化」第 3 步）。
//
// 问题：清扫规则早就有了（24h 过期 + 512 MiB LRU，见 conversion.Cache.Sweep），
// 但**触发点错** —— 只在 bridge 启动时跑一次 ⇒
//
//	桌面端是**长驻**进程（可能连开一天）⇒ 启动那一次扫完，运行期攒下的产物
//	永远占着磁盘 ⇒ 「512 MiB 预算」在运行期**从不生效**（规则形同虚设）。
//
// 这与进程/落盘两族的病根同款：**收尾动作挂在了「只发生一次的时刻」上**
// （启动），而不是挂在「资源的作用域」上（应用作用域 ⇒ 周期性）。
//
// ⇒ 改成「启动扫一次 + 运行期按周期扫」；清扫失败只打一行 stderr（缓存是派生物，
// 扫不成功最多多占点磁盘，不该报错给用户）。

import (
	"sync"
	"time"
)

// previewSweepInterval 运行期清扫周期（30 分钟）。
//
// 为什么是 30 分钟：清扫要遍历缓存根目录、每个 key 目录一次 WalkDir 统计大小
// ⇒ 太频繁会与正常使用抢 IO；30 分钟足够把「512 MiB 预算」的偏差压住
// （最坏情况：半小时内新增 512 MiB ⇒ 瞬时翻倍 ⇒ 可接受）。
const previewSweepInterval = 30 * time.Minute

// previewCacheSweeper 预览缓存的周期清扫器。
type previewCacheSweeper struct {
	stopOnce sync.Once
	stopCh   chan struct{}
}

// startPreviewCacheSweeper 起周期清扫协程（首扫交给调用方：启动流程里已经扫过一次，
// 这里只负责「之后每隔一段时间再扫」）。
//
// 返回值可 nil 安全：stop() 对 nil 接收者是空操作（manager 由多处字面量构造，
// 不保证起过这个协程）。
func startPreviewCacheSweeper(sweep func()) *previewCacheSweeper {
	return startPreviewCacheSweeperEvery(sweep, previewSweepInterval)
}

// startPreviewCacheSweeperEvery 同上，但周期由调用方给（测试用小周期）。
func startPreviewCacheSweeperEvery(sweep func(), every time.Duration) *previewCacheSweeper {
	if sweep == nil || every <= 0 {
		return nil
	}
	s := &previewCacheSweeper{stopCh: make(chan struct{})}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				sweep()
			}
		}
	}()
	return s
}

// stop 停清扫协程（幂等；应用关闭时调）。
func (s *previewCacheSweeper) stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
}
