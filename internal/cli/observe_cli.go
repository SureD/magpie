package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/observe"
	"github.com/yetone/magpie/internal/proc"
)

const observeAddr = "127.0.0.1:3431"

// observeCmd runs the trimmed web edition: the proxy, and a local page for
// session text, per-request usage and trends. It does not open the desktop
// shell or start skill sync, memory, or the library hub.
func observeCmd(args []string) error {
	addr := observeAddr
	lan := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--addr" && i+1 < len(args):
			i++
			addr = args[i]
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--lan":
			lan = true
		default:
			return fmt.Errorf("magpie observe: unknown %q · magpie observe [--addr host:port] [--lan]", a)
		}
	}
	if lan && addr == observeAddr {
		_, port, _ := net.SplitHostPort(observeAddr)
		addr = "0.0.0.0:" + port
	}
	key, err := observeKey()
	if err != nil {
		return err
	}
	proc.UserPath()
	proc.KeepReaping(context.Background())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gw := gateway.New()
	gwErr := make(chan error, 1)
	go func() { gwErr <- gw.ListenAndServe(ctx) }()
	select {
	case err := <-gwErr:
		fmt.Println(amber.Render("!"), "网关没有由这个进程监听：", err)
		fmt.Println(muted.Render("  若已有 magpie 占用端口，观测页仍可读同一份本地记录。"))
	case <-time.After(400 * time.Millisecond):
		fmt.Println(green.Render("●"), "网关", bold.Render(gateway.URL()))
	}
	page, err := observe.Start(addr, key)
	if err != nil {
		return err
	}
	defer page.Close()
	host, port, _ := net.SplitHostPort(page.Addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	link := "http://" + net.JoinHostPort(host, port) + "/?k=" + key
	fmt.Println(green.Render("●"), "观测页", bold.Render(link))
	fmt.Println(muted.Render("  只绑定本机，除非使用了 --lan。链接里的密钥是这一次运行的登录。"))
	fmt.Println(muted.Render("  对话默认不记录。在「记录」页确认后才会保存正文。"))
	fmt.Println(muted.Render("  Ctrl-C 停止。不会改写已有 providers.json 以外的用户数据，除非你在页面上保存。"))
	if lan {
		fmt.Println(amber.Render("!"), "页面已对网络开放。持有链接的人可以查看已记录的对话和管理 provider 密钥。")
	}
	public := advertisedURL()
	fmt.Println(muted.Render("  应用把 base URL 指到网关，并自己带上会话 ID："))
	fmt.Println(muted.Render("  OpenAI     "), public+"/v1/chat/completions", " 与 ", public+"/v1/responses")
	fmt.Println(muted.Render("  Anthropic  "), public+"/v1/messages")
	fmt.Println(muted.Render("  Gemini     "), public+"/v1beta/models/{model}:generateContent")
	fmt.Println(muted.Render("  请求头 X-Magpie-Session: <客户端的会话 ID>"))
	fmt.Println(muted.Render("  可选 X-Magpie-Project: <项目名>。没有会话头的请求不会被记成对话。"))
	<-ctx.Done()
	return nil
}

func observeKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("MAGPIE_OBSERVE_KEY")); k != "" {
		if len(k) < 16 {
			return "", fmt.Errorf("MAGPIE_OBSERVE_KEY must be at least 16 characters")
		}
		return k, nil
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
