package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	indexHeader = "# Project Memory"
	topicFence  = "---"
)

var indexBullet = regexp.MustCompile(`^- \[([^\]]+)\]\(([^)]+)\)(?: — (.*))?$`)

func renderIndex(idx *Index) (string, int) {
	var b strings.Builder
	b.WriteString(indexHeader)
	b.WriteString("\n\n")
	for _, e := range idx.Entries {
		summary := e.Summary
		if summary == "" {
			summary = e.Title
		}
		fmt.Fprintf(&b, "- [%s](topics/%s.md) — %s\n", e.Name, e.Name, escapeInline(summary))
	}
	if len(idx.Entries) == 0 {
		b.WriteString("- (no topics yet)\n")
	}
	if ctx := strings.Trim(idx.Context, "\n"); ctx != "" {
		b.WriteString("\n## Context\n\n")
		b.WriteString(ctx)
		b.WriteString("\n")
	}
	out := b.String()
	return out, len(out)
}

func renderIndexEntry(e IndexEntry) string {
	summary := e.Summary
	if summary == "" {
		summary = e.Title
	}
	return fmt.Sprintf("- [%s](topics/%s.md) — %s", e.Name, e.Name, escapeInline(summary))
}

func parseIndex(repoID, path, raw string, truncated bool) *Index {
	idx := &Index{RepoID: repoID, Path: path, Raw: raw, Truncated: truncated, Lines: countLines(raw), Bytes: len(raw)}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == indexHeader || strings.HasPrefix(line, "## Context") {
			continue
		}
		m := indexBullet.FindStringSubmatch(line)
		if len(m) != 4 {
			continue
		}
		name, err := validateTopicName(m[1])
		if err != nil || m[2] != "topics/"+name+".md" {
			continue
		}
		idx.Entries = append(idx.Entries, IndexEntry{Name: name, Title: name, Summary: m[3]})
	}
	idx.TopicCount = len(idx.Entries)
	return idx
}

type topicMeta struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Scope       Scope    `yaml:"scope"`
	Kind        string   `yaml:"kind,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	CreatedAt   string   `yaml:"created_at,omitempty"`
	UpdatedAt   string   `yaml:"updated_at,omitempty"`
}

func renderTopic(t *Topic) string {
	meta := topicMeta{
		ID: t.Name, Title: t.Title, Description: t.Summary, Scope: t.Scope, Kind: t.Kind, Tags: t.Tags,
		CreatedAt: formatTime(t.Created), UpdatedAt: formatTime(t.Updated),
	}
	metaBytes, _ := yaml.Marshal(meta)
	var b strings.Builder
	b.WriteString(topicFence + "\n")
	b.Write(metaBytes)
	b.WriteString(topicFence + "\n\n")
	fmt.Fprintf(&b, "# %s — %s\n\n", t.Name, t.Title)
	if body := strings.Trim(t.Body, "\n"); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String()
}

func parseTopic(name, path, raw string) *Topic {
	t := &Topic{Name: name, Scope: ScopeRepo, Body: raw}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == topicFence {
		i = 1
		var yamlLines []string
		for ; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == topicFence {
				i++
				break
			}
			yamlLines = append(yamlLines, lines[i])
		}
		var meta topicMeta
		if err := yaml.Unmarshal([]byte(strings.Join(yamlLines, "\n")), &meta); err == nil {
			if n, err := validateTopicName(meta.ID); err == nil {
				t.Name = n
			}
			t.Title, t.Summary, t.Tags = meta.Title, meta.Description, meta.Tags
			t.Kind = meta.Kind
			if s, err := ValidateScope(meta.Scope); err == nil {
				t.Scope = s
			}
			t.Created, t.Updated = parseTime(meta.CreatedAt), parseTime(meta.UpdatedAt)
		}
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "# ") {
		heading := strings.TrimSpace(strings.TrimSpace(lines[i])[2:])
		if _, title, ok := strings.Cut(heading, " — "); ok {
			t.Title = strings.TrimSpace(title)
		} else {
			t.Title = heading
		}
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	t.Body = normalizeBody(strings.Join(lines[i:], "\n"))
	return t
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(timeLayout)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func escapeInline(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "---", "—")
	return strings.TrimSpace(s)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
