package core

// P2-04 复审 B1：Usage.Add 跨轮累加必须保留有价性（全有价 → true），
// 否则 AgentEnd.Usage.Cost.Priced 永远 false，Hub 任务成本三态塌成「未知」。
import "testing"

func TestCostAddPreservesPriced(t *testing.T) {
	var u Usage
	first := Usage{Input: 10, Cost: Cost{Total: 100, Priced: true, Source: CostSourceBuiltin}}
	u = u.Add(first)
	if !u.Cost.Priced || u.Cost.Source != CostSourceBuiltin || u.Cost.Total != 100 {
		t.Fatalf("first add = %+v", u.Cost)
	}
	u = u.Add(Usage{Input: 5, Cost: Cost{Total: 50, Priced: true, Source: CostSourceBuiltin}})
	if !u.Cost.Priced || u.Cost.Total != 150 || u.Input != 15 {
		t.Fatalf("all priced add = %+v", u.Cost)
	}
	// 有一轮无价 → 聚合无价（不能把未知当 0）。
	u = u.Add(Usage{Input: 1, Cost: Cost{Total: 0, Priced: false, Source: CostSourceNone}})
	if u.Cost.Priced || u.Cost.Source != "" {
		t.Fatalf("mixed add = %+v", u.Cost)
	}
	// 普通工具结果恒为空 Usage：不能把已获得的有价性 AND 掉（P2-04 复审 B1）。
	var w Usage
	w = w.Add(Usage{Input: 10, Cost: Cost{Total: 100, Priced: true, Source: CostSourceBuiltin}})
	w = w.Add(Usage{})
	if !w.Cost.Priced || w.Cost.Source != CostSourceBuiltin || w.Cost.Total != 100 || w.Input != 10 {
		t.Fatalf("empty round erased priced/source: %+v", w)
	}
	// 有 token 但无价仍须 AND 成 false（空轮短路不能把未知当 0）。
	w = w.Add(Usage{Input: 1, Cost: Cost{Total: 0, Priced: false, Source: CostSourceNone}})
	if w.Cost.Priced || w.Cost.Source != "" {
		t.Fatalf("unpriced round must clear priced: %+v", w.Cost)
	}
	// 来源不同也清空 Source（但全有价时 Priced 仍为 true）。
	var v Usage
	v = v.Add(Usage{Cost: Cost{Total: 1, Priced: true, Source: CostSourceBuiltin}})
	v = v.Add(Usage{Cost: Cost{Total: 1, Priced: true, Source: CostSourceUpstream}})
	if !v.Cost.Priced || v.Cost.Source != "" || v.Cost.Total != 2 {
		t.Fatalf("mixed source = %+v", v.Cost)
	}
}
