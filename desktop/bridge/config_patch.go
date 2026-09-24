// config_patch.go —— 配置文件的「读 → 改自己那部分 → 原子写」公共底座
// （契约 docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md §6）。
//
// 为什么需要它：settings.json 这类文件是**多写者**的（Electron 写 provider/settings 段、
// bridge 写 mcpServers/hooks 段），任何一方"整文件重写"都会抹掉别人的段；而"截断后写"
// 遇到崩溃/被杀就留下半截文件（读方解析失败 → 该段配置全部消失）。因此本文件提供：
//
//   - patchJSONFile：读对象 → 只改指定键 → 目录 0700 + 同目录临时文件 0600 + rename 原子替换；
//     文件不存在按 {} 起步；**解析失败一律报错拒绝**（覆盖坏文件等于让用户丢掉整份配置）。
//   - 未识别字段合并辅助：模型不认识的键（未来字段/第三方字段）按名/按位置保留原样，
//     模型认识的键一律以模型为准（否则"清空 env/args"这类操作清不掉）。
//
// 并发语义（诚实交代，与契约 §6 一致）：两个写者各自"读 → 改 → 原子写"，冲突窗口是
// 微秒级 —— 最坏结果是后写方覆盖前写方那一份改动（与第三方工具直接改文件同类风险）。
// 本进程内的写者（bridge 命令）由 writeMu 串行化，不会自相覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// patchDelete patchJSONFile 的删除标记：某键的值传它就**删除该键**
// （区别于写空对象/空数组 —— 契约 §5.2「空 map → 删除该键」）。
type patchDelete struct{}

// deleteKey patchDelete 的值形态（写 patch 时用它标记删除）。
var deleteKey = patchDelete{}

// writeMu 串行化本进程内的 patchJSONFile 写：命令目前走单一 stdin 循环串行派发，
// 但"读 → 改 → 写"不串行就丢改动这件事不该依赖那个实现细节（将来把命令放 goroutine 里
// 就会静默丢段）；跨进程（Electron 写同一个 settings.json）的冲突见文件头注释。
var writeMu sync.Mutex

// patchJSONFile 只改 JSON 对象里的指定键，其余键原样保留；原子写（0600）。
//   - 文件不存在 → 按 {} 起步（首次写该段即创建）；
//   - 文件存在但不是合法 JSON 对象 → 返回错误且**不写**（用户可手修）；
//   - patch 值为 deleteKey → 删除该键；
//   - patch 为空 → 直接返回（不创建空文件）。
func patchJSONFile(path string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	obj := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil {
		if uerr := json.Unmarshal(raw, &obj); uerr != nil {
			return fmt.Errorf("%s 不是有效 JSON（%v），已拒绝覆盖", filepath.Base(path), uerr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s 失败: %v", path, err)
	}
	for k, v := range patch {
		if _, del := v.(patchDelete); del {
			delete(obj, k)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("序列化 %s 失败: %v", k, err)
		}
		obj[k] = b
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicBridge(path, append(out, '\n'))
}

// readJSONObject 读 JSON 对象（缺文件/坏文件 → nil；调用方按"没有可继承的旧内容"处理）。
// 与 patchJSONFile 的差别是**不报错**：这里只用于"能不能继承未识别字段"，
// 坏文件由随后的 patchJSONFile 统一拒绝（诊断只出一次，不重复两遍）。
func readJSONObject(path string) map[string]json.RawMessage {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

// mergeUnknownByName 按**名**保留未识别字段（mcpServers 这类「名字 → 条目」的段）：
// 名字没变的条目，prev 里存在、known 里没有的键原样补回；known 里的键一律以 model 为准。
// 名字不在 prev 里（新增条目）或 model 里没有的（已删除条目）都不参与。
func mergeUnknownByName(model, prev map[string]json.RawMessage, known map[string]bool) map[string]json.RawMessage {
	if len(prev) == 0 || len(model) == 0 {
		return model
	}
	out := make(map[string]json.RawMessage, len(model))
	for name, raw := range model {
		prevRaw, ok := prev[name]
		if !ok {
			out[name] = raw
			continue
		}
		merged, ok := mergeUnknownFields(raw, prevRaw, known)
		if !ok {
			out[name] = raw
			continue
		}
		out[name] = merged
	}
	return out
}

// mergeUnknownFields 把 prev 对象里 known 之外的键补进 model 对象（按位置/按名对齐后的单条）。
// known 里的键以 model 为准（含"模型里没有 = 清空"）；顺序 JSON 语义无关，键序由编码器排序。
func mergeUnknownFields(model, prev json.RawMessage, known map[string]bool) (json.RawMessage, bool) {
	var m, p map[string]json.RawMessage
	if json.Unmarshal(model, &m) != nil || json.Unmarshal(prev, &p) != nil {
		return nil, false
	}
	changed := false
	for k, v := range p {
		if known[k] {
			continue // 模型认识的键：以模型为准
		}
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
		changed = true
	}
	if !changed {
		return nil, false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return b, true
}

// rawMessageMap 把 map[string]any（命令 payload 解析结果）转成 map[string]json.RawMessage
// （未识别字段合并的输入形态）。map 为 nil → nil。
func rawMessageMap(v any) map[string]json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// rawSection 读某文件里某个「名字 → 条目」对象段的原始条目（未识别字段保留的输入）。
// 缺文件/坏文件/该段不存在 → nil（调用方按"没有可继承的旧内容"处理，不报错：
// 真要写盘时的坏文件由 patchJSONFile 统一拒绝，诊断只出一次）。
func rawSection(path, key string) map[string]json.RawMessage {
	raw, ok := readJSONObject(path)[key]
	if !ok {
		return nil
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// writeFileAtomicBridge 同目录临时文件 0600 + rename（读侧永远看到完整文件）。
// 与 hooks/manage.go 的同名逻辑一致（不同包不能共用；两处都要求"目录 0700、文件 0600"）。
func writeFileAtomicBridge(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".go-code-*.tmp")
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
