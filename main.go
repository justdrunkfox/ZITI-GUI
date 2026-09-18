package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"fyne.io/systray"
)

var version = "0.1.0"

func main() {
	var (
		identityDir = flag.String("identity-dir", "", "каталог идентичностей (по умолчанию — из конфига или /opt/openziti/etc/identities)")
		service     = flag.String("service", "", "имя systemd-сервиса туннелера (по умолчанию ziti-edge-tunnel.service)")
		once        = flag.Bool("once", false, "один раз собрать статус и вывести JSON (без трея)")
		showVer     = flag.Bool("version", false, "показать версию и выйти")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("ziti-gui", version)
		return
	}

	cfg := loadConfig()
	if *identityDir != "" {
		cfg.IdentityDir = *identityDir
	}
	if *service != "" {
		cfg.Service = *service
	}
	a := newApp(cfg)

	if *once {
		st := a.poll()
		enc, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(enc))
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if a.quit != nil {
			a.quit()
		} else {
			os.Exit(0)
		}
	}()

	// single-instance: если уже запущен — просим открыть окно и выходим
	ln, first := bindInstanceLock()
	if !first {
		fmt.Println("ziti-gui уже запущен — открыл окно")
		return
	}
	if ln != nil {
		serveInstanceLock(ln, a)
		defer ln.Close()
	}

	// Имя пункта в трее (Id/Title у StatusNotifierItem): до Run() — иначе
	// библиотека впишет туда "systray_<pid>".
	systray.SetTitle("ZITI-GUI")
	systray.Run(a.onTrayReady, nil)
}
