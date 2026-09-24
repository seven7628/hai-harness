package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// manage.go —— hooks 配置的**可编辑读写面**（设置面板「钩子」页的数据源与回写口）。
//
// 与 config.go 的分工：
//   - config.go（Load/mergeFile）是**运行期合并视角**：多层追加合并、校验、告警，只为执行；
//   - 本文件是**单文件编辑视角**：读出一个文件的 hooks 段原样模型（含未识别键、告警线索），
//     供面板展示/编辑，再把编辑结果严格校验后写回。
//
// 两条硬约束（契约 docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md §5.6）：
//  1. **拒绝式校验**：非法事件名/handler 类型/负超时/非法 matcher 正则一律返回错误，
//     不静默丢弃（面板直接展示中文原因）；运行期才告警太晚 —— 用户会以为配好了。
//  2. **未识别条目字段保留**：事件下的 matcher 组、组内的 handler 按**位置**对齐合并 ——
//     位置没变时，文件里有、模型不认识的键（未来字段/第三方字段）原样保留；模型认识的键
//     一律以模型为准（否则"清空 env/args"这类操作清不掉）。顶层未识别键**不保留**
//     （整段重写，UI 以 unknownKeys 提示，见契约 §9）。

// Section 是 hooks 段的**可编辑**形态（与内部 section 同形，字段/JSON tag 一一对应）。
//
// 为什么导出：设置面板要原样 round-trip（读出来 → 用户改 → 写回去），而内部 section
// 是私有类型；两者同形保证「写回」不丢字段（Group/Handler 已带 json tag，可直接复用）。
type Section struct {
	DisableAll         bool               `json:"disableAllHooks"`
	Disabled           []string           `json:"disabled"`
	ReadClaudeSettings *bool              `json:"readClaudeSettings"`
	AllowProjectHooks  bool               `json:"allowProjectHooks"`
	RequireTrust       bool               `json:"requireTrust"`
	Events             map[string][]Group `json:"events"`
}

// Source 一次 ReadSource 的结果：面板展示信息 + 回写时「未识别字段保留」的合并输入。
//
// Present/OK 与 mcp.LayerState 同语义（两处面板一致）：
//   - Present = 文件存在且读到了内容（好坏都算）；缺文件 = false + OK=true（正常：尚未配置）；
//   - OK      = 本次内容有效可用（能解析出模型）。
type Source struct {
	Path        string   // 文件绝对路径
	Present     bool     // 文件存在且读到了内容
	OK          bool     // 内容有效（能解析）
	Err         string   // 解析/读取失败原因（OK=false 时）
	UnknownKeys []string // hooks 段（key="" 时是文件根）顶层未识别键，升序
	Body        *Section // hooks 段模型（永远非 nil：缺文件/缺段 = 空模型）

	// prevGroups 上一次读取到的 matcher 组原始 JSON（事件名 → 按位置的组）。
	// 只在同一包的 WriteSource 里用：位置不变的条目把它认识的键补回写盘内容。
	// 跨文件传入（prev.Path != path）会被忽略——否则会把 A 文件的第三方字段搬到 B 文件。
	prevGroups map[string][]rawGroup
}

// rawGroup 一个 matcher 组的原始对象（保留未识别键用）。
type rawGroup struct {
	obj      map[string]json.RawMessage   // 组的顶层键值
	handlers []map[string]json.RawMessage // 组内 handler 的顶层键值（按位置）
}

// groupKnownKeys / handlerKnownKeys 模型认识的键：这些键一律以模型为准（含"清空"）。
// 未列出的键 = 未识别（保留原样）。与 Group/Handler 的 json tag 严格对应，改 tag 必须同步。
var (
	groupKnownKeys = map[string]bool{"matcher": true, "hooks": true}

	handlerKnownKeys = map[string]bool{
		"type": true, "name": true, "command": true, "timeout": true, "async": true,
		"statusMessage": true, "failureMode": true, "additionalContextLimit": true, "enabled": true,
	}
)

// newSection 空模型（非 nil 切片/映射：面板拿到的是 [] / {}，不是 null）。
func newSection() *Section {
	return &Section{Disabled: []string{}, Events: map[string][]Group{}}
}

// normalize 把模型补成群组完整的形态（空切片 → []，空映射 → {}），并把
// readClaudeSettings 显式化（缺省 = 读，与运行期 Load 的判定一致）——面板把 null
// 当 false 显示就会得到「看起来关着其实开着」的错位，显式 bool 消除这个歧义。
func (s *Section) normalize() *Section {
	if s == nil {
		return newSection()
	}
	out := *s
	if out.Disabled == nil {
		out.Disabled = []string{}
	}
	if out.Events == nil {
		out.Events = map[string][]Group{}
	}
	if out.ReadClaudeSettings == nil {
		on := true
		out.ReadClaudeSettings = &on
	}
	return &out
}

// ReadSource 读取一个 hooks 配置来源并解析成模型。
//
//	key != ""  → 文件是 settings.json 形态，先解一层（如 "hooks"）；
//	key == ""  → 整个文件就是 hooks 段（{ws}/.go-code/hooks.json 形态）。
//
// 两种写法都认（与运行期 mergeFile 一致）：go-code 原生 {"events": {...}, …} 与
// Claude Code 扁平 {"PreToolUse": [...]}；同时存在时按"先 events 后扁平"追加。
func ReadSource(path, key string) Source {
	src := Source{Path: path, Body: newSection(), prevGroups: map[string][]rawGroup{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			src.OK = true // 缺文件是正常情况（尚未配置）：present=false + 空模型
			return src
		}
		src.Present = true // 文件在但读不到（权限/IO）：与"缺文件"区分开
		src.Err = "读取失败: " + err.Error()
		return src
	}
	src.Present = true
	if strings.TrimSpace(string(raw)) == "" {
		// 空/纯空白 = 半写或未初始化。与 mcp 的"沿用上次有效"不同：hooks 这一侧没有
		// last-good 层，硬猜成"没配 hooks"会让用户以为配置没了；报出来 + 拒绝覆盖更诚实。
		src.Err = "文件内容为空（视为半写/未初始化；重写会丢弃原内容，已拒绝）"
		return src
	}

	body := raw
	if key != "" {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			src.Err = "JSON 解析失败: " + err.Error()
			return src
		}
		b, ok := envelope[key]
		if !ok {
			src.OK = true // 文件有效但没有该段：空模型（面板显示"未配置"，可直接新增）
			return src
		}
		body = b
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		src.Err = fmt.Sprintf("%s 不是 JSON 对象: %v", sectionLabel(key), err)
		return src
	}
	// 标量字段逐个解析（不用内部 section 整体解）：错误信息能带上字段名，
	// 面板直接展示「哪一项写错了」而不是 encoding/json 的结构体字段路径。
	if err := decodeSectionScalars(obj, src.Body); err != nil {
		src.Err = fmt.Sprintf("%s %v", sectionLabel(key), err)
		return src
	}
	src.Body = src.Body.normalize() // "disabled": null → []（面板拿到的一定是可迭代形态）

	// 事件：形态 1（events 包裹）优先，形态 2（事件名扁平挂载）追加 —— 与 mergeFile 同序。
	rawEvents := map[string]json.RawMessage{}
	if ev, ok := obj["events"]; ok {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ev, &m); err != nil {
			src.Err = fmt.Sprintf("%s 的 events 不是 JSON 对象: %v", sectionLabel(key), err)
			return src
		}
		for name, v := range m {
			rawEvents[name] = v
		}
	}
	for k, v := range obj {
		if _, isEvent := Anchors[Event(k)]; isEvent {
			rawEvents[k] = v
		}
	}
	unknown := map[string]bool{}
	for k := range obj {
		if !knownTopKey(k) {
			unknown[k] = true // 顶层未识别键（provider 段之外的第三方键）
		}
	}
	for _, name := range sortedKeys(rawEvents) {
		if _, ok := Anchors[Event(name)]; !ok {
			// 我们不认识的事件名（未来版本/第三方字段）：不进可编辑模型 —— 否则面板
			// 既渲染不出中文说明，保存时又会被严格校验拒绝（用户被卡住，无从删除）。
			// 与顶层未识别键同档：列进 unknownKeys（"保存后不保留"对用户可见）。
			unknown[name] = true
			continue
		}
		groups, err := parseGroups(rawEvents[name])
		if err != nil {
			src.Err = fmt.Sprintf("%s 事件 %s 的组解析失败: %v", sectionLabel(key), name, err)
			return src
		}
		src.Body.Events[name] = append(src.Body.Events[name], groups...)
		src.prevGroups[name] = append(src.prevGroups[name], rawGroupsOf(rawEvents[name])...)
	}

	// 顶层未识别键（含未知事件名）：整段重写时不保留 —— UI 据此提示用户。
	for k := range unknown {
		src.UnknownKeys = append(src.UnknownKeys, k)
	}
	sort.Strings(src.UnknownKeys)
	src.OK = true
	return src
}

// decodeSectionScalars 解析五个开关字段（类型非法 → 带字段名的可读错误）。
func decodeSectionScalars(obj map[string]json.RawMessage, out *Section) error {
	for k, dst := range map[string]any{
		"disableAllHooks":    &out.DisableAll,
		"disabled":           &out.Disabled,
		"readClaudeSettings": &out.ReadClaudeSettings,
		"allowProjectHooks":  &out.AllowProjectHooks,
		"requireTrust":       &out.RequireTrust,
	} {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("字段 %s 类型非法: %v", k, err)
		}
	}
	return nil
}

// WriteSource 把模型写回文件：严格校验 → 目录 0700 + 同目录临时文件 0600 + rename 原子写。
//
// key != "" 时只替换该段，文件的**其他顶层键原样保留**（settings.json 里有 provider 等
// 别的段，整文件重写会抹掉它们）；key == "" 时整文件 = hooks 段。
// prev 传 ReadSource 的返回值（同一文件）时，条目级未识别字段按位置保留；传零值 = 不保留。
// 校验失败返回可读中文原因（面板直接展示），且**不写盘**（拒绝式，不静默丢）。
func WriteSource(path, key string, body *Section, prev Source) error {
	normalized, err := validateSection(body)
	if err != nil {
		return err
	}
	sectionJSON, err := marshalSection(normalized, path, prev)
	if err != nil {
		return err
	}
	if key == "" {
		return writeFileAtomic(path, append(sectionJSON, '\n'))
	}

	// settings.json 形态：读出整个对象，只改自己那段（保留其他键）。
	obj := map[string]json.RawMessage{}
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if uerr := json.Unmarshal(raw, &obj); uerr != nil {
			// 坏文件拒绝写：覆盖它等于让用户丢掉整个配置（他还能手修）。
			return fmt.Errorf("%s 不是有效 JSON（%v），已拒绝覆盖", filepath.Base(path), uerr)
		}
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("读取 %s 失败: %v", path, rerr)
	}
	obj[key] = sectionJSON
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'))
}

// —— 校验与归一化 ——

// validateSection 严格校验（拒绝式）并归一化：
//   - 事件名必须是 Anchors 里的锚点；
//   - handler type 空或 command（大小写不敏感；空统一补成 "command"）；
//   - timeout >= 0；
//   - matcher 非法正则拒绝（运行期才告警太晚：用户会以为配好了）；
//   - 空 handler / 空组 / 空事件清掉（不写空壳）；
//   - 命令不能为空（运行期该条必被跳过 → 存下来就是死配置）。
func validateSection(in *Section) (*Section, error) {
	s := in.normalize()
	out := newSection()
	out.DisableAll = s.DisableAll
	out.Disabled = append([]string{}, s.Disabled...)
	out.ReadClaudeSettings = s.ReadClaudeSettings
	out.AllowProjectHooks = s.AllowProjectHooks
	out.RequireTrust = s.RequireTrust

	for _, name := range sortedEventNames(s.Events) {
		groups := s.Events[name]
		if _, ok := Anchors[Event(name)]; !ok {
			return nil, fmt.Errorf("未知事件 %s（支持 %s）", name, strings.Join(anchorNames(), " / "))
		}
		keptGroups := make([]Group, 0, len(groups))
		for gi := range groups {
			g := groups[gi]
			if err := validateMatcher(name, gi+1, g.Matcher); err != nil {
				return nil, err
			}
			keptHandlers := make([]Handler, 0, len(g.Hooks))
			for hi := range g.Hooks {
				h := g.Hooks[hi]
				where := fmt.Sprintf("%s 第 %d 组第 %d 条", name, gi+1, hi+1)
				if k := h.Kind(); k != "command" {
					return nil, fmt.Errorf("%s：暂不支持的 handler 类型 %s（当前只支持 command）", where, h.Type)
				}
				if h.Timeout < 0 {
					return nil, fmt.Errorf("%s：超时不能为负数（%v）", where, h.Timeout)
				}
				if strings.TrimSpace(h.Command) == "" {
					return nil, fmt.Errorf("%s：命令不能为空", where)
				}
				if strings.TrimSpace(h.Type) == "" {
					h.Type = "command" // 写入统一显式化（读入时空 = command，两者等价）
				}
				keptHandlers = append(keptHandlers, h)
			}
			if len(keptHandlers) == 0 {
				continue // 空组清掉
			}
			g.Hooks = keptHandlers
			g.Matcher = strings.TrimSpace(g.Matcher)
			keptGroups = append(keptGroups, g)
		}
		if len(keptGroups) == 0 {
			continue // 空事件清掉
		}
		out.Events[name] = keptGroups
	}
	return out, nil
}

// validateMatcher matcher 三态之一：空/"*"/".*"（全匹配）、纯候选列表（alnum）、正则。
// 只有"正则"档需要编译校验 —— 前两档不编译（与运行期 matches 的判定口径一致）。
func validateMatcher(event string, index int, matcher string) error {
	m := strings.TrimSpace(matcher)
	if m == "" || m == "*" || m == ".*" || alnum.MatchString(m) {
		return nil
	}
	if _, err := regexp.Compile(m); err != nil {
		return fmt.Errorf("%s 第 %d 组：matcher 非法正则 %s（%v）", event, index, m, err)
	}
	return nil
}

// —— 序列化（含条目级未识别字段保留）——

// marshalSection 把模型序列化成 hooks 段 JSON。path/prev 用于取上一次的原始条目
// （位置对齐保留未识别字段）；prev.Path != path 时视为"没有上一次"，不合并。
func marshalSection(s *Section, path string, prev Source) ([]byte, error) {
	eventsOut := map[string]json.RawMessage{}
	for name, groups := range s.Events {
		arr := make([]json.RawMessage, 0, len(groups))
		var prevGroups []rawGroup
		if prev.Path == path {
			prevGroups = prev.prevGroups[name]
		}
		for i := range groups {
			gb, err := json.Marshal(groups[i])
			if err != nil {
				return nil, err
			}
			var pg *rawGroup
			if i < len(prevGroups) {
				pg = &prevGroups[i]
			}
			merged, err := mergeGroupKnown(gb, pg)
			if err != nil {
				return nil, err
			}
			arr = append(arr, merged)
		}
		ev, err := json.Marshal(arr)
		if err != nil {
			return nil, err
		}
		eventsOut[name] = ev
	}
	// 显式结构体（不用 map）：字段序固定 = 配置开关在前、events 在后，人读文件时更顺；
	// events 内的组是 json.RawMessage（已含合并后的未识别字段），键序由编码器排序确定。
	out := struct {
		DisableAll         bool                       `json:"disableAllHooks"`
		Disabled           []string                   `json:"disabled"`
		ReadClaudeSettings *bool                      `json:"readClaudeSettings"`
		AllowProjectHooks  bool                       `json:"allowProjectHooks"`
		RequireTrust       bool                       `json:"requireTrust"`
		Events             map[string]json.RawMessage `json:"events"`
	}{
		DisableAll:         s.DisableAll,
		Disabled:           s.Disabled,
		ReadClaudeSettings: s.ReadClaudeSettings,
		AllowProjectHooks:  s.AllowProjectHooks,
		RequireTrust:       s.RequireTrust,
		Events:             eventsOut,
	}
	return json.MarshalIndent(out, "", "  ")
}

// mergeGroupKnown 组级 + handler 级未识别字段保留（按位置）。
// prev 为 nil = 该位置在文件里没有对应条目（新增/移位）→ 原样用模型。
func mergeGroupKnown(model json.RawMessage, prev *rawGroup) (json.RawMessage, error) {
	if prev == nil {
		return model, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(model, &m); err != nil {
		return model, nil
	}
	for k, v := range prev.obj {
		if groupKnownKeys[k] {
			continue // 模型认识的键一律以模型为准（否则"清空 matcher"清不掉）
		}
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
	var hooks []json.RawMessage
	if raw, ok := m["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err == nil {
			for i := range hooks {
				if i >= len(prev.handlers) {
					break
				}
				merged, err := mergeHandlerKnown(hooks[i], prev.handlers[i])
				if err != nil {
					return nil, err
				}
				hooks[i] = merged
			}
			if b, err := json.Marshal(hooks); err == nil {
				m["hooks"] = b
			}
		}
	}
	return json.Marshal(m)
}

// mergeHandlerKnown handler 级未识别字段保留（同组内按位置对齐）。
func mergeHandlerKnown(model json.RawMessage, prev map[string]json.RawMessage) (json.RawMessage, error) {
	if len(prev) == 0 {
		return model, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(model, &m); err != nil {
		return model, nil
	}
	for k, v := range prev {
		if handlerKnownKeys[k] {
			continue
		}
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// rawGroupsOf 解析某事件的原始组数组（含组内 handler 的原始对象）。
// 解析不了时返回 nil：这类文件在 ReadSource 里已经报错，走不到这里。
func rawGroupsOf(rawEvent json.RawMessage) []rawGroup {
	var arr []json.RawMessage
	if err := json.Unmarshal(rawEvent, &arr); err != nil {
		return nil
	}
	out := make([]rawGroup, 0, len(arr))
	for _, g := range arr {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(g, &obj); err != nil {
			out = append(out, rawGroup{})
			continue
		}
		rg := rawGroup{obj: obj}
		if raw, ok := obj["hooks"]; ok {
			var hs []json.RawMessage
			if err := json.Unmarshal(raw, &hs); err == nil {
				for _, h := range hs {
					var ho map[string]json.RawMessage
					if err := json.Unmarshal(h, &ho); err != nil {
						ho = map[string]json.RawMessage{}
					}
					rg.handlers = append(rg.handlers, ho)
				}
			}
		}
		out = append(out, rg)
	}
	return out
}

// parseGroups 解析一个事件的组数组（整型/字符串等非法形态 → 错误）。
func parseGroups(rawEvent json.RawMessage) ([]Group, error) {
	var groups []Group
	if err := json.Unmarshal(rawEvent, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// —— 小工具 ——

// writeFileAtomic 原子写：目录 0700、同目录临时文件 0600 + rename。
// 读侧永远看到完整文件（半写窗口只存在于临时文件里）；失败清理临时文件。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".hooks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name) // rename 成功后不存在，清理调用无害
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// sectionLabel 错误信息里的段名（key="" 时是整个文件）。
func sectionLabel(key string) string {
	if key == "" {
		return "hooks 文件"
	}
	return key + " 段"
}

// knownTopKey 顶层已识别键：事件名（锚点）+ 五个配置键。
// 其余键 = 未识别（unknownKeys 提示，保存后不保留）。
func knownTopKey(k string) bool {
	if _, ok := Anchors[Event(k)]; ok {
		return true
	}
	switch k {
	case "events", "disableAllHooks", "disabled", "readClaudeSettings", "allowProjectHooks", "requireTrust":
		return true
	}
	return false
}

// anchorNames 锚点事件名（升序；错误信息与面板展示用）。
func anchorNames() []string {
	out := make([]string, 0, len(Anchors))
	for ev := range Anchors {
		out = append(out, string(ev))
	}
	sort.Strings(out)
	return out
}

// sortedEventNames 事件名的确定顺序（map 遍历随机 → 校验/写盘报错要可复现）。
// 先按锚点表顺序，其余（非法名）按字典序排在后面。
func sortedEventNames(events map[string][]Group) []string {
	anchorOrder := map[string]int{}
	for i, n := range anchorNames() {
		anchorOrder[n] = i
	}
	out := make([]string, 0, len(events))
	for name := range events {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oki := anchorOrder[out[i]]
		oj, okj := anchorOrder[out[j]]
		switch {
		case oki && okj:
			return oi < oj
		case oki != okj:
			return oki // 认识的锚点在前，未知名在后（错误信息先报要紧的）
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// sortedKeys map[string]json.RawMessage 的有序键（写盘/解析确定顺序）。
func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
