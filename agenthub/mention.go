package agenthub

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// MaxMentionsPerMessage 是单条消息最多唤醒的智能体数（@all 另算，见 D14）。
const MaxMentionsPerMessage = 3

const (
	mentionAllAlias      = "all"
	mentionEveryoneAlias = "所有人"
	mentionHumanAlias    = "你"
	mentionUserAlias     = "user"
)

// ErrMentionAllForbidden 表示发送者没有 @all 权限；调用方不得降级成普通 @。
var ErrMentionAllForbidden = errors.New("agenthub: mention_all forbidden")

// MentionResult 是严格解析器的结果；正文本身不被修改。
type MentionResult struct {
	Mentions     []AgentID `json:"mentions,omitempty"`
	MentionAll   bool      `json:"mention_all,omitempty"`
	HumanMention bool      `json:"human_mention,omitempty"` // @你/@user：高亮人类，不唤醒模型
	Unknown      []string  `json:"unknown,omitempty"`       // 未命中成员；只用于警告，不触发
	Dropped      int       `json:"dropped,omitempty"`       // 超过 3 个后丢弃的唯一成员数
}

type mentionCandidate struct {
	id    AgentID
	name  string
	runes []rune
	fold  string
}

// MentionParser 把正文里的 @Name 解析成结构化 mention。
// 构造时拒绝重名/保留名；解析时跳过 code/URL/email，按最长成员名匹配。
type MentionParser struct {
	candidates   []mentionCandidate
	allAliases   [][]rune
	humanAliases [][]rune
}

// NewMentionParser 用群成员 profile 构造解析器。重名（大小写不敏感）直接报错；
// 已删除的成员不可被 @，跳过。
func NewMentionParser(profiles []*AgentProfile) (*MentionParser, error) {
	p := &MentionParser{
		allAliases:   [][]rune{[]rune(mentionAllAlias), []rune(mentionEveryoneAlias)},
		humanAliases: [][]rune{[]rune(mentionHumanAlias), []rune(mentionUserAlias)},
	}
	seen := make(map[string]AgentID, len(profiles))
	for _, profile := range profiles {
		if profile == nil || profile.Deleted {
			continue
		}
		if profile.ID == "" {
			return nil, fmt.Errorf("%w: mention member has empty id", ErrInvalid)
		}
		if err := validateProfileName(profile.Name); err != nil {
			return nil, fmt.Errorf("%w: mention member %s: %v", ErrInvalid, profile.ID, err)
		}
		name := strings.TrimSpace(profile.Name)
		key := strings.ToLower(name)
		if key == mentionAllAlias || key == mentionEveryoneAlias || key == mentionHumanAlias || key == mentionUserAlias {
			return nil, fmt.Errorf("%w: mention name %q is reserved", ErrInvalid, name)
		}
		if other, ok := seen[key]; ok {
			return nil, fmt.Errorf("%w: duplicate mention name %q (%s and %s)", ErrInvalid, name, other, profile.ID)
		}
		seen[key] = profile.ID
		p.candidates = append(p.candidates, mentionCandidate{
			id:    profile.ID,
			name:  name,
			runes: []rune(name),
			fold:  key,
		})
	}
	sort.Slice(p.candidates, func(i, j int) bool {
		li, lj := len(p.candidates[i].runes), len(p.candidates[j].runes)
		if li != lj {
			return li > lj // 最长匹配优先
		}
		return p.candidates[i].fold < p.candidates[j].fold
	})
	return p, nil
}

// Parse 解析正文；不忽略任何发送者（由调用方自行过滤 self）。
func (p *MentionParser) Parse(text string) MentionResult {
	return p.ParseFrom(text, "")
}

// ParseFrom 解析正文并忽略 sender 对自己的 @（自我 @ 不唤醒、不算 dropped）。
func (p *MentionParser) ParseFrom(text string, sender AgentID) MentionResult {
	res := MentionResult{}
	if p == nil || text == "" {
		return res
	}
	runes := []rune(text)
	masked := maskMentionSkipped(runes)
	seen := make(map[AgentID]bool)
	unknownSeen := make(map[string]bool)

	for i := 0; i < len(runes); {
		if masked[i] || runes[i] != '@' {
			i++
			continue
		}
		if !mentionBoundaryBefore(runes, i) {
			i++
			continue
		}
		start := i + 1
		if start >= len(runes) {
			break
		}

		if n, ok := matchAnyRuneAlias(runes, start, p.allAliases); ok && mentionBoundaryAfter(runes, start+n) {
			res.MentionAll = true
			i = start + n
			continue
		}
		if n, ok := matchAnyRuneAlias(runes, start, p.humanAliases); ok && mentionBoundaryAfter(runes, start+n) {
			res.HumanMention = true
			i = start + n
			continue
		}

		matched := false
		for _, c := range p.candidates {
			end := start + len(c.runes)
			if end > len(runes) || !equalFoldRunes(runes[start:end], c.runes) {
				continue
			}
			if !mentionBoundaryAfter(runes, end) {
				continue
			}
			matched = true
			if c.id != sender && !seen[c.id] {
				seen[c.id] = true
				if len(res.Mentions) < MaxMentionsPerMessage {
					res.Mentions = append(res.Mentions, c.id)
				} else {
					res.Dropped++
				}
			}
			i = end
			break
		}
		if matched {
			continue
		}

		token, n := readUnknownMention(runes, start)
		if token != "" && !unknownSeen[token] {
			unknownSeen[token] = true
			res.Unknown = append(res.Unknown, token)
		}
		if n > 0 {
			i = start + n
		} else {
			i++
		}
	}
	return res
}

// CanMentionAll：人类与 owner 智能体可 @all；member/observer 不可，且不降级（D14）。
func CanMentionAll(isHuman bool, role MemberRole) bool {
	return isHuman || role == RoleOwner
}

// ValidateMentionAll 在解析结果含 @all 时检查发送者权限。
func (r MentionResult) ValidateMentionAll(isHuman bool, role MemberRole) error {
	if !r.MentionAll {
		return nil
	}
	if !CanMentionAll(isHuman, role) {
		return fmt.Errorf("%w: only owner or human can @all", ErrMentionAllForbidden)
	}
	return nil
}

func matchAnyRuneAlias(runes []rune, start int, aliases [][]rune) (int, bool) {
	for _, alias := range aliases {
		end := start + len(alias)
		if end <= len(runes) && equalFoldRunes(runes[start:end], alias) {
			return len(alias), true
		}
	}
	return 0, false
}

func equalFoldRunes(a, b []rune) bool {
	return strings.EqualFold(string(a), string(b))
}

// mentionBoundaryBefore：前面不能是字母/数字/反斜杠/斜杠，也不能是另一个 @。
func mentionBoundaryBefore(runes []rune, at int) bool {
	if at == 0 {
		return true
	}
	r := runes[at-1]
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\\' && r != '/' && r != '@'
}

// mentionBoundaryAfter：后面不能是字母/数字/连字符。
func mentionBoundaryAfter(runes []rune, at int) bool {
	if at >= len(runes) {
		return true
	}
	r := runes[at]
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
}

func readUnknownMention(runes []rune, start int) (string, int) {
	end := start
	for end < len(runes) && !isMentionTokenDelimiter(runes[end]) {
		end++
	}
	token := strings.TrimRight(string(runes[start:end]), ".,;:!?)]}<>\"'，。；：！？、")
	return strings.TrimSpace(token), end - start
}

func isMentionTokenDelimiter(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case ',', '.', ';', ':', '!', '?', ')', ']', '}', '<', '>', '"', '\'', '，', '。', '；', '：', '！', '？', '、':
		return true
	}
	return false
}

// maskMentionSkipped 标出不参与 mention 解析的区间：fenced/inline code、URL、email。
func maskMentionSkipped(runes []rune) []bool {
	masked := make([]bool, len(runes))
	for i := 0; i < len(runes); {
		switch runes[i] {
		case '`':
			n := runLength(runes, i, '`')
			end := findFenceEnd(runes, i+n, '`', n)
			if end < 0 {
				end = len(runes)
			} else {
				end += n
			}
			for k := i; k < end && k < len(runes); k++ {
				masked[k] = true
			}
			i = end
			continue
		case '~':
			n := runLength(runes, i, '~')
			if n >= 3 {
				end := findFenceEnd(runes, i+n, '~', n)
				if end < 0 {
					end = len(runes)
				} else {
					end += n
				}
				for k := i; k < end && k < len(runes); k++ {
					masked[k] = true
				}
				i = end
				continue
			}
		}

		if hasRunePrefix(runes, i, "http://") || hasRunePrefix(runes, i, "https://") || hasRunePrefix(runes, i, "www.") {
			end := i
			for end < len(runes) && !isURLTerminator(runes[end]) {
				end++
			}
			for k := i; k < end; k++ {
				masked[k] = true
			}
			i = end
			continue
		}
		i++
	}
	markEmailTokens(runes, masked)
	return masked
}

func runLength(runes []rune, start int, r rune) int {
	n := 0
	for start+n < len(runes) && runes[start+n] == r {
		n++
	}
	return n
}

func findFenceEnd(runes []rune, start int, r rune, want int) int {
	for i := start; i < len(runes); {
		if runes[i] != r {
			i++
			continue
		}
		n := runLength(runes, i, r)
		if n >= want {
			return i
		}
		i += n
	}
	return -1
}

func hasRunePrefix(runes []rune, at int, prefix string) bool {
	pr := []rune(prefix)
	if at+len(pr) > len(runes) {
		return false
	}
	return equalFoldRunes(runes[at:at+len(pr)], pr)
}

func isURLTerminator(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '<', '>', '"', '\'', ')', ']', '}':
		return true
	}
	return false
}

func markEmailTokens(runes []rune, masked []bool) {
	for i := 0; i < len(runes); {
		for i < len(runes) && unicode.IsSpace(runes[i]) {
			i++
		}
		start := i
		for i < len(runes) && !unicode.IsSpace(runes[i]) {
			i++
		}
		if isEmailToken(runes[start:i]) {
			for k := start; k < i; k++ {
				masked[k] = true
			}
		}
	}
}

func isEmailToken(token []rune) bool {
	at := -1
	for i, r := range token {
		if r == '@' {
			at = i
			break
		}
	}
	if at <= 0 || at >= len(token)-1 {
		return false
	}
	// 邮箱 local part 的最后一个字符必须是常见 local 字符；这避免把 "(@Writer)" 当邮箱。
	last := token[at-1]
	if !(unicode.IsLetter(last) || unicode.IsDigit(last) || strings.ContainsRune("._%+-", last)) {
		return false
	}
	for _, r := range token[at+1:] {
		if r == '.' {
			return true
		}
	}
	return false
}
