package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/console"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
)

func (a *app) uiCommand() *cobra.Command {
	var port int
	var open bool
	cmd := &cobra.Command{Use: "ui", Short: "启动轻量本地管理台（任务、记忆、运行、Agent 配置）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.serveUI(cmd.Context(), port, open)
	}}
	cmd.Flags().IntVar(&port, "port", 8787, "本地端口，0 自动选择")
	cmd.Flags().BoolVar(&open, "open", false, "打开系统浏览器")
	return cmd
}

func (a *app) serveUI(ctx context.Context, port int, open bool) error {
	return a.serveUIControlled(ctx, port, open, nil)
}

func (a *app) serveUIControlled(ctx context.Context, port int, open bool, restart func(context.Context, console.RestartRequest) (func(), error)) error {
	if port < 0 || port > 65535 {
		return core.Fail("invalid_input", "--port must be 0..65535")
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return core.Fail("unavailable", "cannot listen on local UI port: %v", err)
	}
	defer listener.Close()
	resolveSkills := cachedSkillResolver()
	var resumeTask func(context.Context, string, int) (any, error)
	if restart != nil {
		resumeTask = taskResumeControl(a.home)
	}
	server, err := console.Open(ctx, console.Options{Home: a.home, ConfigPath: a.configPath, Version: Version, Build: observation.ExecutableBuild(), Host: listener.Addr().String(), ResolveSkills: resolveSkills, AgentConfig: newAgentConfigEditor(a.home, a.configPath, resolveSkills), Restart: restart, ResumeTask: resumeTask})
	if err != nil {
		return err
	}
	defer server.Close()
	url := server.URL()
	_, _ = fmt.Fprintf(a.out, "memgov 本地管理台 · Agent 声明可编辑，其余只读\n%s\n数据目录：%s\n关闭浏览器标签页不影响服务；统一服务的 Ctrl-C / service stop 停止全部模块。\n", url, a.home)
	if open {
		if err := openUIBrowser(url); err != nil {
			_, _ = fmt.Fprintln(a.errOut, "无法自动打开浏览器，请使用上面的地址。")
		}
	}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(ctx)
		case <-done:
		}
	}()
	err = httpServer.Serve(listener)
	close(done)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func cachedSkillResolver() func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error) {
	return backgroundSkillResolver(resolveClaudeSkills)
}

func backgroundSkillResolver(resolve func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error)) func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error) {
	type cached struct {
		at       time.Time
		skills   []core.RuntimeSkill
		err      error
		scanning bool
	}
	cache := map[string]cached{}
	var mu sync.Mutex
	return func(policy core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error) {
		if policy.Inherit != "executor" && len(policy.Paths) == 0 {
			return []core.RuntimeSkill{}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		policy.Resolved = nil // Applied digests are outputs, not discovery inputs.
		key := core.Digest(policy)
		if hit, ok := cache[key]; ok && (hit.scanning || time.Since(hit.at) < 30*time.Second) {
			return hit.skills, hit.err
		}
		pending := core.Fail("unavailable", "技能清单扫描中；页面稍后自动刷新，编辑请稍后重新预览。清单不代表持续会话加载结果")
		cache[key] = cached{at: time.Now(), err: pending, scanning: true}
		go func() {
			skills, err := resolve(policy)
			mu.Lock()
			cache[key] = cached{at: time.Now(), skills: skills, err: err}
			mu.Unlock()
		}()
		return nil, pending
	}
}

func openUIBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Run()
	}
	return exec.Command(name, url).Run()
}
