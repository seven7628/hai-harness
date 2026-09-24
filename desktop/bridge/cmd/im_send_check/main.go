package main

// im_send_check：IM 联调自检工具（真实凭据）
//
// 用法：go run ./desktop/bridge/cmd/im_send_check ~/.go-code/im-config.json oc_xxx
// 读取 im-config.json 中第一个 enabled feishu gateway，向指定 chat_id 发一条文本消息，
// 验证 Send 链路（token 缓存 / 长连接 / 消息权限）是否可用。
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/im/feishu"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: im_send_check <im-config.json> <chat_id>")
		os.Exit(2)
	}
	cfgPath, chatID := os.Args[1], os.Args[2]
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Println("read config:", err)
		os.Exit(1)
	}
	var cfg im.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Println("parse config:", err)
		os.Exit(1)
	}
	var gwCfg *im.GatewayConfig
	for i := range cfg.Gateways {
		g := &cfg.Gateways[i]
		if g.Enabled && g.Type == "feishu" {
			gwCfg = g
			break
		}
	}
	if gwCfg == nil {
		fmt.Println("no enabled feishu gateway in config")
		os.Exit(1)
	}
	fmt.Printf("gateway: %s (%s)\n", gwCfg.ID, gwCfg.Type)
	gw, err := feishu.New(gwCfg.Config)
	if err != nil {
		fmt.Println("New:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		fmt.Println("Start:", err)
		os.Exit(1)
	}
	defer gw.Stop(context.Background())
	time.Sleep(5 * time.Second) // 等待长连接就绪
	mid, err := gw.Send(ctx, im.Chat{Gateway: "feishu", ChatID: chatID}, im.OutMessage{
		Kind: im.KindText,
		Text: "✅ go-code IM 联调自检：这条消息由真实凭据发出（sendText 链路验证）",
	})
	if err != nil {
		fmt.Println("Send FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("Send OK, message_id:", mid)
}
