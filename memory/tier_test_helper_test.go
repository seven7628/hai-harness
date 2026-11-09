package memory

// fullCfg 返回 A 档（全量注入）配置，是既有注入断言的基线。
//
// 背景（2026-11 P1）：产品的默认注入形态从"整份索引"切成"热点清单"，
// 而 memory/index_test.go 里大量断言是"注入文本 == 标题 + 索引正文 + 说明段"。
// 那些断言测的是**渲染**，不是**默认值**——所以让它们显式声明 A 档，
// 而不是靠"默认恰好还是 full"混过去。默认值是哪个，由 tier_test.go 单独测。
func fullCfg() Config { return Config{Injection: InjectionFull} }
