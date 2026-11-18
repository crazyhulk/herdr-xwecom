// herdr-wecom bridges Herdr agents and a WeCom (企业微信) AI bot.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crazyhulk/herdr-xwecom/internal/bridge"
	"github.com/crazyhulk/herdr-xwecom/internal/config"
	"github.com/crazyhulk/herdr-xwecom/internal/herdr"
	"github.com/crazyhulk/herdr-xwecom/internal/proc"
	"github.com/crazyhulk/herdr-xwecom/internal/wecom"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon()
	case "startup":
		err = startup()
	case "action":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		err = action(os.Args[2])
	case "setup-pane":
		err = setupPane()
	case "logs-pane":
		err = logsPane()
	case "configure":
		err = configure(os.Args[2:])
	case "version":
		fmt.Println("herdr-wecom", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-wecom:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: herdr-wecom <daemon|startup|action NAME|setup-pane|logs-pane|configure|version>
  configure --bot-id ID --secret SECRET [--allow USERID,...]   write config.json non-interactively
  actions: setup start stop restart status pair logs send-test`)
}

func paths() proc.Paths { _, s := config.Dirs(); return proc.Paths{StateDir: s} }

func notify(title, body string) {
	_ = herdr.New().Notify(context.Background(), title, body)
	fmt.Println(title + ": " + body)
}

func startup() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Configured() {
		notify("WeCom", "未配置：运行 "+config.PluginID+".setup")
		return nil
	}
	return startDaemon(false)
}

func startDaemon(announce bool) error {
	p := paths()
	if pid, ok := p.Running(); ok {
		if announce {
			notify("WeCom", fmt.Sprintf("已在运行 (pid %d)", pid))
		}
		return nil
	}
	pid, err := p.Spawn()
	if err != nil {
		return err
	}
	if announce {
		notify("WeCom", fmt.Sprintf("已启动 (pid %d)", pid))
	}
	return nil
}

func action(name string) error {
	p := paths()
	switch name {
	case "setup":
		return openPane("setup")
	case "logs":
		return openPane("logs")
	case "start":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if !cfg.Configured() {
			notify("WeCom", "未配置：先运行 setup")
			return nil
		}
		return startDaemon(true)
	case "stop":
		if err := p.Stop(); err != nil {
			return err
		}
		notify("WeCom", "已停止")
	case "restart":
		if err := p.Stop(); err != nil {
			return err
		}
		return startDaemon(true)
	case "status":
		cfg, _ := config.Load()
		st, _ := config.LoadState()
		pid, running := p.Running()
		users := config.HomeUsers(cfg, st)
		msg := fmt.Sprintf("configured=%v running=%v pid=%d users=%v", cfg.Configured(), running, pid, users)
		notify("WeCom status", msg)
	case "pair":
		st, err := config.LoadState()
		if err != nil {
			return err
		}
		_ = st.Update(func(s *config.State) { s.PairCode = "" })
		code := st.EnsurePairCode()
		notify("WeCom 配对码", "在企业微信中给机器人发送：/pair "+code)
	case "send-test":
		return sendTest()
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func openPane(entry string) error {
	root := os.Getenv("HERDR_PLUGIN_ROOT")
	args := []string{"plugin", "pane", "open", "--focus", "--plugin", config.PluginID, "--entrypoint", entry}
	if root != "" {
		args = append(args, "--env", "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	for _, k := range []string{"HERDR_PLUGIN_CONFIG_DIR", "HERDR_PLUGIN_STATE_DIR", "HERDR_PLUGIN_ROOT"} {
		if v := os.Getenv(k); v != "" {
			args = append(args, "--env", k+"="+v)
		}
	}
	out, err := exec.Command("herdr", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("herdr plugin pane open: %v: %s", err, out)
	}
	return nil
}

// sendTest goes through the daemon when it runs (one connection per bot).
func sendTest() error {
	if _, ok := paths().Running(); ok {
		st, _ := config.LoadState()
		_ = st.Update(func(s *config.State) {})
		return os.WriteFile(paths().StateDir+"/send-test.request", []byte(time.Now().Format(time.RFC3339)), 0o600)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := config.LoadState()
	if err != nil {
		return err
	}
	users := config.HomeUsers(cfg, st)
	if len(users) == 0 {
		notify("WeCom", "没有接收人：先 /pair 配对或在 config.json 设置 allow_users")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := wecom.New(cfg.BotID, cfg.Secret, log, nil)
	go c.Run(ctx)
	if err := c.WaitConnected(ctx); err != nil {
		return err
	}
	for _, u := range users {
		if err := c.Send(ctx, u, "🧪 Herdr WeCom 测试消息"); err != nil {
			return err
		}
	}
	notify("WeCom", "测试消息已发送")
	return nil
}

func configure(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--bot-id":
			cfg.BotID = args[i+1]
		case "--secret":
			cfg.Secret = args[i+1]
		case "--allow":
			cfg.AllowUsers = splitList(args[i+1])
		case "--home":
			cfg.HomeUsers = splitList(args[i+1])
		default:
			return fmt.Errorf("unknown flag %s", args[i])
		}
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Println("wrote", config.ConfigPath())
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func readLine(r *bufio.Reader, prompt string, hidden bool) string {
	fmt.Print(prompt)
	if hidden {
		_ = exec.Command("stty", "-echo").Run()
		defer func() { _ = exec.Command("stty", "echo").Run(); fmt.Println() }()
	}
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
}

func setupPane() error {
	r := bufio.NewReader(os.Stdin)
	cfg, _ := config.Load()
	fmt.Println("== Herdr ⇄ 企业微信 设置 ==")
	fmt.Println("在企业微信管理后台创建「智能机器人」，选择 API 模式 / 长连接，获取 Bot ID 与 Secret。")
	fmt.Println()
	if cfg.BotID != "" {
		fmt.Printf("当前 Bot ID: %s（直接回车保留）\n", cfg.BotID)
	}
	if v := readLine(r, "Bot ID: ", false); v != "" {
		cfg.BotID = v
	}
	if v := readLine(r, "Secret（输入隐藏，回车保留）: ", true); v != "" {
		cfg.Secret = v
	}
	if !cfg.Configured() {
		fmt.Println("缺少 Bot ID 或 Secret。按回车退出。")
		r.ReadString('\n')
		return nil
	}
	p := paths()
	_, wasRunning := p.Running()
	if wasRunning {
		_ = p.Stop() // one connection per bot: verifying would kick the daemon
	}
	fmt.Print("验证凭证… ")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	err := wecom.Verify(ctx, cfg.BotID, cfg.Secret)
	cancel()
	if err != nil {
		fmt.Println("失败:", err)
		fmt.Println("按回车退出。")
		r.ReadString('\n')
		return nil
	}
	fmt.Println("成功")
	if err := config.Save(cfg); err != nil {
		return err
	}
	st, err := config.LoadState()
	if err != nil {
		return err
	}
	_ = st.Update(func(s *config.State) { s.PairCode = "" })
	code := st.EnsurePairCode()
	if err := startDaemon(false); err != nil {
		fmt.Println("启动守护进程失败:", err)
	}
	fmt.Println()
	fmt.Println("守护进程已启动。在企业微信中私聊该机器人，发送：")
	fmt.Println()
	fmt.Printf("    /pair %s\n\n", code)
	fmt.Println("配对后即可收到 agent 通知并远程控制。发送 /help 查看命令。")
	fmt.Println("按回车关闭。")
	r.ReadString('\n')
	return nil
}

func logsPane() error {
	cmd := exec.Command("tail", "-n", "200", "-F", paths().Log())
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// ------------------------------------------------------------------ daemon

type dispatcher struct{ b *bridge.Bridge }

func (d *dispatcher) OnMessage(m wecom.Message)     { d.b.OnMessage(m) }
func (d *dispatcher) OnCardClick(c wecom.CardClick) { d.b.OnCardClick(c) }

func runDaemon() error {
	p := paths()
	if pid, ok := p.Running(); ok && pid != os.Getpid() {
		return fmt.Errorf("daemon already running (pid %d)", pid)
	}
	if err := p.WritePID(); err != nil {
		return err
	}
	defer p.RemovePID()

	level := slog.LevelInfo
	if os.Getenv("HERDR_WECOM_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Configured() {
		return errors.New("not configured (bot_id/secret)")
	}
	st, err := config.LoadState()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := herdr.New()
	d := &dispatcher{}
	wc := wecom.New(cfg.BotID, cfg.Secret, log.With("c", "wecom"), d)
	cfgFn := func() config.Config {
		c, err := config.Load()
		if err != nil {
			return cfg
		}
		return c
	}
	b := bridge.New(h, wc, cfgFn, st, log.With("c", "bridge"))
	d.b = b
	log.Info("daemon start", "version", version, "pid", os.Getpid(), "socket", h.Path)

	errc := make(chan error, 1)
	go func() { errc <- wc.Run(ctx) }()
	go b.Run(ctx)
	go watchSendTest(ctx, p, wc, cfgFn, st, log)

	// Exit when the Herdr socket stays unreachable (server stopped).
	var downSince time.Time
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("daemon stop", "reason", "signal")
			return nil
		case err := <-errc:
			if errors.Is(err, wecom.ErrKicked) {
				_ = h.Notify(context.Background(), "WeCom", "连接被其他客户端顶掉（同一机器人只允许一个长连接），守护进程退出")
			}
			log.Info("daemon stop", "reason", err)
			return nil
		case <-t.C:
			if _, err := h.Ping(ctx); err != nil {
				if downSince.IsZero() {
					downSince = time.Now()
				}
				if time.Since(downSince) > 60*time.Second {
					log.Info("daemon stop", "reason", "herdr socket gone")
					return nil
				}
			} else {
				downSince = time.Time{}
			}
		}
	}
}

func watchSendTest(ctx context.Context, p proc.Paths, wc *wecom.Client, cfg func() config.Config, st *config.State, log *slog.Logger) {
	req := p.StateDir + "/send-test.request"
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := os.Stat(req); err != nil {
			continue
		}
		_ = os.Remove(req)
		for _, u := range config.HomeUsers(cfg(), st) {
			if err := wc.Send(ctx, u, "🧪 Herdr WeCom 测试消息"); err != nil {
				log.Warn("send-test failed", "user", u, "err", err)
			} else {
				log.Info("send-test delivered", "user", u)
			}
		}
	}
}
