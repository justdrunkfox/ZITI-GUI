package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- exec-хелперы

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// systemctl без привилегий — is-active/show доступны любому пользователю.
func (a *app) systemctl(ctx context.Context, args ...string) (string, error) {
	return runCmd(ctx, "systemctl", args...)
}

// systemctl через pkexec — start/stop/restart требуют polkit-авторизацию
// (после «Настроить доступ» срабатывает правило и пароль не спрашивается).
func (a *app) systemctlPriv(ctx context.Context, args ...string) (string, error) {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return "", errors.New("pkexec не найден в системе")
	}
	c, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	full := append([]string{"systemctl"}, args...)
	cmd := exec.CommandContext(c, "pkexec", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// zitiBin ищет бинарь туннелера.
func (a *app) zitiBin() string {
	if a.cfg.ZitiBin != "" {
		return a.cfg.ZitiBin
	}
	for _, p := range []string{
		"/opt/openziti/bin/ziti-edge-tunnel",
		"/usr/bin/ziti-edge-tunnel",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("ziti-edge-tunnel"); err == nil {
		return p
	}
	return "ziti-edge-tunnel"
}

// zitiClient — клиентская команда туннелера (dump, add, on_off_identity, ...)
// поверх IPC-сокета запущенного экземпляра; выполняется от имени пользователя.
func (a *app) zitiClient(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, a.zitiBin(), args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var (
	verOnce  sync.Once
	verValue string
)

func (a *app) zitiVersion() string {
	verOnce.Do(func() {
		out, err := runCmd(context.Background(), a.zitiBin(), "version")
		if err != nil {
			verValue = "не удалось получить"
			return
		}
		verValue = strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	})
	return verValue
}

// svcUser — от кого работает сервис (User= из unit, обычно ziti).
func (a *app) svcUser() string {
	out, err := a.systemctl(context.Background(), "show", "-p", "User", "--value", a.cfg.Service)
	if err == nil {
		if u := strings.TrimSpace(out); u != "" {
			return u
		}
	}
	return "ziti"
}

// ------------------------------------------------------------- идентичности

// listIdentityFiles — содержимое каталога идентичностей (если читается).
func (a *app) listIdentityFiles() map[string]string {
	files := map[string]string{}
	entries, err := os.ReadDir(a.cfg.IdentityDir)
	if err != nil {
		return files
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext := filepath.Ext(name); ext != ".json" && ext != ".jwt" {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if base == "config" { // служебный конфиг пакета, не идентичность
			continue
		}
		files[base] = filepath.Join(a.cfg.IdentityDir, name)
	}
	return files
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(filepath.Base(s))
	s = strings.TrimSuffix(s, filepath.Ext(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = fmt.Sprintf("identity-%d", time.Now().Unix())
	}
	return out
}
