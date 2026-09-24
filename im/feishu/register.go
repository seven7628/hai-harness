package feishu

import (
	"github.com/seven7628/hai-harness/im"
)

func init() {
	// 配置即注册：type=feishu → New 构造器（docs/IM_INTEGRATION.md §3.4）
	im.Register(Type, New)
	// Schema 显式登记（UI 动态表单数据源；不依赖构造器探测——空配置构造会失败）
	im.RegisterSchema(Type, Schema())
}

// Schema 返回飞书 Gateway 的配置 Schema（UI 动态表单数据源，docs/IM_INTEGRATION.md §9）。
// 由 init 登记到 im 注册表；独立函数（非实例方法）便于登记。
func Schema() im.GatewaySchema {
	return im.GatewaySchema{
		Type:        Type,
		Name:        "飞书",
		Description: "企业自建应用机器人：长连接接收、卡片审批、话题支持",
		Groups: []im.FieldGroup{
			{
				Title: "基础",
				Fields: []im.Field{
					{
						Key:      "app_id",
						Label:    "App ID",
						Type:     im.TypeString,
						Required: true,
						Help:     "飞书开放平台 → 开发者后台 → 凭证与基础信息",
					},
					{
						Key:      "app_secret",
						Label:    "App Secret",
						Type:     im.TypePassword,
						Required: true,
						Secret:   true,
						Help:     "同上；保存后不回显",
					},
					{
						Key:      "mode",
						Label:    "接收模式",
						Type:     im.TypeSelect,
						Required: true,
						Default:  "websocket",
						Options: []im.Option{
							{Value: "websocket", Label: "长连接（推荐，无需公网）"},
							{Value: "webhook", Label: "Webhook（需公网 + 加密配置）"},
						},
						Help: "本地桌面端建议长连接",
					},
				},
			},
			{
				Title: "高级",
				Fields: []im.Field{
					{
						Key:      "encrypt_key",
						Label:    "Encrypt Key",
						Type:     im.TypePassword,
						Required: false,
						Secret:   true,
						Advanced: true,
						Help:     "事件加密 key；Webhook 模式必填",
					},
					{
						Key:      "verify_token",
						Label:    "验证令牌",
						Type:     im.TypePassword,
						Required: false,
						Secret:   true,
						Advanced: true,
						Help:     "事件订阅验证令牌（可选）",
					},
					{
						Key:      "base_url",
						Label:    "自定义 API 域名",
						Type:     im.TypeString,
						Required: false,
						Advanced: true,
						Default:  "https://open.feishu.cn",
						Help:     "默认 open.feishu.cn；私有化部署才需要改",
					},
				},
			},
		},
	}
}
