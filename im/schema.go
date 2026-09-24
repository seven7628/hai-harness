package im

// FieldType 配置字段类型（UI 动态表单渲染依据）。
type FieldType string

const (
	TypeString   FieldType = "string"   // 单行文本
	TypePassword FieldType = "password" // 密码/密钥（不回显，仅显示"已设置"）
	TypeSelect   FieldType = "select"   // 下拉
	TypeBool     FieldType = "bool"     // 开关
	TypeTextArea FieldType = "textarea" // 多行
	TypeNumber   FieldType = "number"   // 数字
	TypeJSON     FieldType = "json"     // JSON 编辑（高级）
)

// Option select 选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field 单个配置字段元数据。
type Field struct {
	Key         string    `json:"key"`   // 配置键（config 里的字段名）
	Label       string    `json:"label"` // UI 显示名（i18n key 或原文）
	Type        FieldType `json:"type"`
	Required    bool      `json:"required"`
	Placeholder string    `json:"placeholder,omitempty"`
	Help        string    `json:"help,omitempty"`    // 说明文字（含获取指引）
	Options     []Option  `json:"options,omitempty"` // select 用
	Default     any       `json:"default,omitempty"`
	Secret      bool      `json:"secret,omitempty"`   // 敏感：保存后不回显、提供"测试连接"
	Advanced    bool      `json:"advanced,omitempty"` // 高级区（默认折叠）
}

// FieldGroup 字段分组（可选；不分组则单区）。
type FieldGroup struct {
	Title  string  `json:"title"`
	Fields []Field `json:"fields"`
}

// GatewaySchema 一个 Gateway 类型的配置描述（注册表提供，UI 动态渲染）。
type GatewaySchema struct {
	Type        string       `json:"type"`        // feishu / telegram / …
	Name        string       `json:"name"`        // 展示名
	Description string       `json:"description"` // 一句话说明
	Groups      []FieldGroup `json:"groups,omitempty"`
	// RequiredFields 配置完成度检查（供 UI 显示"还差什么"）。
	RequiredFields []string `json:"required_fields,omitempty"`
}

// AllFields 返回全部字段（不分组时用）。
func (s GatewaySchema) AllFields() []Field {
	var out []Field
	for _, g := range s.Groups {
		out = append(out, g.Fields...)
	}
	return out
}

// Required 返回必填字段 key 列表。
func (s GatewaySchema) Required() []string {
	if len(s.RequiredFields) > 0 {
		return s.RequiredFields
	}
	var out []string
	for _, f := range s.AllFields() {
		if f.Required {
			out = append(out, f.Key)
		}
	}
	return out
}
