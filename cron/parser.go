// Package cron 进程内定时任务：LLM 可创建/查询/更新/删除的调度任务，
// 到点由宿主调度器投递为新 Session 执行（trigger 方式 = 从对话框输入换成定时输入）。
//
// 本文件：自实现的标准 5 字段 cron 表达式解析（不依赖系统 cron / 三方库）。
// 字段序：minute hour day-of-month month day-of-week
//   - "*"        任意值（"?" 视作 "*" 的别名，兼容常见写法）
//   - "n"        单个值
//   - "a-b"      闭区间
//   - "a-b/n"    区间步进（等价 */n 的区间限定形式）
//   - "*/n"      从最小值起每 n 个
//   - "a,b,c"    列表（上述元素可混用，如 "0,15,30,45"、"1-5"、"*/5"）
//
// 月份支持 1-12（以及 JAN-DEC）；星期支持 0-7（0 与 7 均为周日，以及 SUN-SAT）。
// DOM/DOW 语义（标准）：两者都受限时取「或」（任一匹配即触发）；其一为 * 时看另一个。
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// field 某一位的匹配规则：anyMatch（*）或显式 values 集合。
type field struct {
	anyMatch bool
	values   map[int]bool
}

func newField() field { return field{values: map[int]bool{}} }

// matches 判断 value 是否命中该位。
func (f field) matches(v int) bool { return f.anyMatch || f.values[v] }

// Spec 已编译的 5 字段 cron 表达式。
type Spec struct {
	raw        string
	minute     field
	hour       field
	dayOfMonth field
	month      field
	dayOfWeek  field
}

// Parse 编译 cron 表达式；非法返回 error。
func Parse(expr string) (*Spec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron: 需要 5 个字段(M H DoM Mon DoW)，得到 %d: %q", len(parts), expr)
	}
	s := &Spec{raw: expr}
	var err error
	if s.minute, err = parseField(parts[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron: minute 字段: %w", err)
	}
	if s.hour, err = parseField(parts[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron: hour 字段: %w", err)
	}
	if s.dayOfMonth, err = parseField(parts[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron: day-of-month 字段: %w", err)
	}
	if s.month, err = parseField(parts[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron: month 字段: %w", err)
	}
	if s.dayOfWeek, err = parseField(parts[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("cron: day-of-week 字段: %w", err)
	}
	return s, nil
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// parseValue 解析单个 token 为数字（支持名字映射）；未知名字报错。
func parseValue(tok string, names map[string]int) (int, error) {
	if names != nil {
		for name, v := range names {
			if strings.EqualFold(tok, name) {
				return v, nil
			}
		}
	}
	return strconv.Atoi(tok)
}

// parseField 编译一位：min..max 为该位合法范围。
func parseField(raw string, min, max int, names map[string]int) (field, error) {
	f := newField()
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return f, fmt.Errorf("空元素")
		}
		if part == "*" || part == "?" {
			f.anyMatch = true
			continue
		}
		base, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			base, step = part[:i], 1
			st, err := strconv.Atoi(part[i+1:])
			if err != nil || st <= 0 {
				return f, fmt.Errorf("非法步进 %q", part)
			}
			step = st
		}
		loV, hiV := 0, 0
		if base == "*" || base == "?" {
			// "* /n" 等价 "min-max/n"
			loV, hiV = min, max
		} else {
			lo, hi := base, base
			if i := strings.Index(base, "-"); i >= 0 {
				lo, hi = base[:i], base[i+1:]
			}
			var err error
			loV, err = parseValue(lo, names)
			if err != nil {
				return f, fmt.Errorf("非法值 %q", lo)
			}
			hiV = loV
			if hi != base {
				hiV, err = parseValue(hi, names)
				if err != nil {
					return f, fmt.Errorf("非法值 %q", hi)
				}
			}
		}
		if loV < min || hiV > max || loV > hiV {
			return f, fmt.Errorf("超出范围 [%d-%d]: %q", min, max, part)
		}
		for v := loV; v <= hiV; v += step {
			f.values[v] = true
		}
	}
	return f, nil
}

// Match 判断 t 是否命中（DOM/DOW 或语义）。t 会取整到分钟。
func (s *Spec) Match(t time.Time) bool {
	t = t.Truncate(time.Minute)
	if !s.minute.matches(t.Minute()) || !s.hour.matches(t.Hour()) || !s.month.matches(int(t.Month())) {
		return false
	}
	domRestricted := !s.dayOfMonth.anyMatch
	dowRestricted := !s.dayOfWeek.anyMatch
	domOK := s.dayOfMonth.matches(t.Day())
	// 归一 dow：Sunday = 0 或 7 等价。
	dow := int(t.Weekday()) // 0=Sunday
	dowOK := s.dayOfWeek.matches(dow) || s.dayOfWeek.matches(dow+7) || s.dayOfWeek.matches(dow-7)
	if domRestricted && dowRestricted {
		return domOK || dowOK
	}
	if domRestricted {
		return domOK
	}
	if dowRestricted {
		return dowOK
	}
	return true // 两者都为 * → 任意日
}

// NextAfter 返回严格晚于 t 的下一次命中时间（分钟级）。返回 ok=false 表示
// 找不到（理论上不会，除非表达式不可达，如 2 月 30 日）。
func (s *Spec) NextAfter(t time.Time) (time.Time, bool) {
	cand := t.Truncate(time.Minute).Add(time.Minute)
	// 上限：8 年逐分钟扫描（覆盖 2/29 等稀有表达式的最坏情况）。
	const maxIter = 366 * 24 * 60 * 8
	for i := 0; i < maxIter; i++ {
		if s.Match(cand) {
			return cand, true
		}
		cand = cand.Add(time.Minute)
	}
	return time.Time{}, false
}

// Raw 返回原始表达式文本。
func (s *Spec) Raw() string { return s.raw }
