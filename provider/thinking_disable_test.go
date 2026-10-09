package provider

import "testing"

// TestCanDisableThinking 关思考能力判定（压缩等辅助请求的省时省费开关）。
//
// 这条判定的**契约**是「与 provider 侧实际发送行为一致」：CanDisableThinking=true
// ⇒ 显式关思考（Thinking=false / effort=none）能被各 thinkingFormat 正常表达；
// false ⇒ 显式关思考要么无意义（非推理模型）、要么会让请求 400（强制思考模型）。
//
// 两层的分工（**别搞错它们各自保护什么**）：
//   - 逐类样本（本函数后半 + TestCanDisableThinkingMatchesResolveThinkingRequests）：
//     真正钉模型级行为的地方，改实现/改元数据会红。
//   - 全表扫描（下面这段）：只是 **smoke assertion** —— 它比较的是「实现的逻辑镜像」
//     （want 的表达式与 CanDisableThinking 内部那个 if 同构），所以对 providers.json
//     的任何改动都不敏感（got 与 want 一起变）。它的价值仅限于：数据源加载失败、
//     表被清空、或某类模型彻底消失时报警，并顺手给出表中两类模型的占比。
//     想靠它抓住"某个模型的标注被改错"是做不到的 —— 那种回归由样本层负责。
func TestCanDisableThinking(t *testing.T) {
	reg := NewRegistry()
	models := BuiltinModels()
	if len(models) == 0 {
		t.Fatal("内置注册表为空（providers.json 未加载？）")
	}

	// Smoke assertion：实现与自身口径一致 + 两类模型都还在表里。非模型级保护（见上）。
	var canCount, cannotCount int
	for _, info := range models {
		got := reg.CanDisableThinking(info.Provider, info.ID)
		want := !info.ThinkingForceOn && !info.ThinkingDefaultOff
		if got != want {
			t.Errorf("%s/%s: CanDisableThinking = %v, want %v（force_on=%v default_off=%v reasoning=%v）",
				info.Provider, info.ID, got, want, info.ThinkingForceOn, info.ThinkingDefaultOff, info.Reasoning)
		}
		if got {
			canCount++
		} else {
			cannotCount++
		}
	}
	if canCount == 0 || cannotCount == 0 {
		t.Fatalf("扫描结果失衡：可关 %d / 不可关 %d —— 内置表应同时覆盖思考模型与非推理模型", canCount, cannotCount)
	}

	// 逐类样本：典型模型 + 关键边界（可关 / 强制关不掉 / 非推理模型 / 未知模型）。
	reasoning := reg.CanDisableThinking("deepseek", "deepseek-v4-flash") // deepseek 形态，可发 disabled
	toggle := reg.CanDisableThinking("zhipu", "glm-4.6")                 // toggle-only，只发 thinking.type
	forced := reg.CanDisableThinking("zhipu", "glm-5.3")                 // ThinkingForceOn：disabled 会 400
	nonReason := reg.CanDisableThinking("openai", "gpt-4o")              // 非推理模型：没有思考可关
	unknown := reg.CanDisableThinking("custom", "my-own-model")          // 未知模型：无从判断 → 不可关
	emptyProvider := reg.CanDisableThinking("", "deepseek-v4-flash")     // 跨 provider 按名解析
	family := reg.CanDisableThinking("opencode", "deepseek-v4.1-flash")  // 名称族兜底（网关新版本号）
	// 线上 400 复现样本：opencode Go 网关的模型不在内置表，曾因判成"可关"而发
	// thinking=false 被上游拒（"Reasoning is mandatory ... cannot be disabled"）。
	stepFree := reg.CanDisableThinking("opencode", "step5-preview-free")

	if !reasoning {
		t.Error("deepseek-v4-flash 应可关思考（thinking.type=disabled）")
	}
	if !toggle {
		t.Error("glm-4.6 应可关思考（toggle-only：只发开关）")
	}
	if forced {
		t.Error("glm-5.3 不可关思考（ThinkingForceOn：disabled 会 400）")
	}
	if nonReason {
		t.Error("gpt-4o 不可关思考（非推理模型：没有思考可关）")
	}
	if unknown {
		t.Error("未知模型应判定为不可关（无从判断它是否强制思考；错判的代价不对称，见 CanDisableThinking 注释）")
	}
	if stepFree {
		t.Error("opencode/step5-preview-free 不在内置表，应判定为不可关 —— 判成可关就是线上那个 400")
	}
	if !emptyProvider {
		t.Error("空 provider 名应按模型名全局解析（自定义 provider 装配场景）")
	}
	if !family {
		t.Error("网关新版本号经名称族兜底应判定为可关（同族 deepseek 形态）")
	}
}

// TestCanDisableThinkingMatchesResolveThinkingRequests 判定与请求侧发送的一致性：
// CanDisableThinking=true 时，显式关思考的 ThinkingDecision 必须不带
// ThinkingUnsupported（否则关不掉/不该发），且 force-on 模型的 Enabled 不被关掉。
//
// 这守的是「编译期约定之外的运行时漂移」—— 若将来有人在 providers.json 里给某模型加了
// 新标记，两端口径会在这里立刻对不上。
//
// 同样分两层：全表循环是**结构性**断言（两类判定 → 两类请求侧行为，映射关系写死），
// 它对"某个具体模型的标注被改错"不敏感（同步反映同一份数据），真正逐模型约束的
// 是 TestCanDisableThinking 里的硬编码样本。这里验证的是"两条路径不会互相漂移"。
func TestCanDisableThinkingMatchesResolveThinkingRequests(t *testing.T) {
	reg := NewRegistry()
	off := false
	for _, info := range BuiltinModels() {
		td := reg.ResolveThinking(info.Provider, info.ID, &RequestConfig{Thinking: &off})
		can := reg.CanDisableThinking(info.Provider, info.ID)
		switch {
		case info.ThinkingDefaultOff:
			// 非推理模型：判不可关 + 请求侧标记 ThinkingUnsupported（一个思考参数都不发）
			if can || !td.ThinkingUnsupported {
				t.Errorf("%s/%s：非推理模型应「不可关 + ThinkingUnsupported」，得到 can=%v unsupported=%v",
					info.Provider, info.ID, can, td.ThinkingUnsupported)
			}
		case info.ThinkingForceOn:
			// 强制思考：判不可关 + 请求侧 Enabled 保持 true（不发 disabled）
			if can || !td.Enabled {
				t.Errorf("%s/%s：强制思考模型应「不可关 + Enabled」，得到 can=%v enabled=%v",
					info.Provider, info.ID, can, td.Enabled)
			}
		default:
			// 其余：判可关 + 请求侧真正关掉（不发 ThinkingUnsupported）
			if !can || td.ThinkingUnsupported || td.Enabled {
				t.Errorf("%s/%s：应「可关且关得掉」，得到 can=%v unsupported=%v enabled=%v",
					info.Provider, info.ID, can, td.ThinkingUnsupported, td.Enabled)
			}
		}
	}
}
