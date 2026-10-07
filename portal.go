package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// ------------------------------------------------------- выбор файла (portal)

// portalPattern: Type 0 — расширение-суффикс, 1 — MIME-тип.
type portalPattern struct {
	Type    uint32
	Pattern string
}

type portalFilter struct {
	Name     string
	Patterns []portalPattern
}

// portalChooseFile открывает системный диалог выбора файла через
// xdg-desktop-portal (работает на Wayland, без cgo). Возвращает путь или "".
func portalChooseFile(title string) (string, error) {
	// приватное соединение: общее может закрыться при выходе из приложения,
	// и горутина ожидания ответа упадёт с SIGSEGV (было такое)
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		addr = "unix:path=" + filepath.Join(xdgRuntime(), "bus")
	}
	conn, err := dbus.Connect(addr,
		dbus.WithAuth(dbus.AuthExternal(strconv.Itoa(os.Getuid()))))
	if err != nil {
		return "", fmt.Errorf("нет сессии DBus: %w", err)
	}
	defer conn.Close()

	token := fmt.Sprintf("zitigui%d", time.Now().UnixNano())

	// подписка без привязки к пути: сверяемся с handle, который вернёт портал
	// (предсказанный путь request/<sender>/<token> не всегда совпадает)
	match := "type='signal',interface='org.freedesktop.portal.Request',member='Response'"
	if err := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, match).Err; err != nil {
		return "", err
	}
	defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, match)

	sigCh := make(chan *dbus.Signal, 8)
	conn.Signal(sigCh)

	filters := []portalFilter{
		{"JWT", []portalPattern{{0, "*.jwt"}, {0, "*.jfile"}, {0, "*.token"}}},
		{"Все файлы", []portalPattern{{0, "*"}}},
	}
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"accept_label": dbus.MakeVariant("Выбрать"),
		"multiple":     dbus.MakeVariant(false),
		"modal":        dbus.MakeVariant(true),
		"filters":      dbus.MakeVariant(filters),
	}

	obj := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	var handle dbus.ObjectPath
	err = obj.Call("org.freedesktop.portal.FileChooser.OpenFile", 0, "", title, opts).Store(&handle)
	if err != nil {
		return "", err
	}
	log.Printf("portal: диалог открыт, handle=%s", handle)

	deadline := time.After(90 * time.Second)
	for {
		select {
		case sig := <-sigCh:
			// ВАЖНО: godbus кладёт в Name полное "интерфейс.член"
			const wantName = "org.freedesktop.portal.Request.Response"
			if sig.Name != wantName {
				continue
			}
			log.Printf("portal: сигнал Response с пути %s (ожидали %s)", sig.Path, handle)
			if len(sig.Body) < 2 {
				return "", fmt.Errorf("странный ответ портала")
			}
			code, _ := sig.Body[0].(uint32)
			if code != 0 {
				return "", nil // пользователь отменил
			}
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			if results == nil {
				return "", nil
			}
			urisV, ok := results["uris"]
			if !ok {
				return "", nil
			}
			uris, _ := urisV.Value().([]string)
			if len(uris) == 0 {
				return "", nil
			}
			u, err := url.Parse(uris[0])
			if err != nil {
				return "", err
			}
			return u.Path, nil
		case <-deadline:
			return "", fmt.Errorf("таймаут диалога выбора файла")
		}
	}
}

// zenityPickFile — выбор файла через zenity: синхронный, путь в stdout.
func zenityPickFile(title string) (string, error) {
	if _, err := exec.LookPath("zenity"); err != nil {
		return "", err
	}
	out, err := exec.Command("zenity", "--file-selection",
		"--title="+title,
		"--file-filter=JWT | *.jwt *.jfile *.token",
		"--file-filter=Все файлы | *").Output()
	if err != nil {
		// код 1 = отмена пользователем
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// pickFileForEnroll выбирает файл: сначала штатный xdg-desktop-portal,
// zenity — запасной вариант.
func pickFileForEnroll(title string) (string, error) {
	p, err := portalChooseFile(title)
	if err == nil && p != "" {
		log.Printf("picker: портал")
		return p, nil
	}
	if err != nil {
		log.Printf("picker: портал не сработал (%v), пробую zenity", err)
	} else {
		log.Printf("picker: портал — отменено, пробую zenity не буду")
		return "", nil
	}
	return zenityPickFile(title)
}

// ---------------------------------------------------------------- уведомления

func xdgRuntime() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

func firstLinePublic(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func notify(summary, body string) {
	log.Printf("notify: %s — %s", summary, firstLinePublic(body))
	go func() {
		conn, err := dbus.SessionBus()
		if err != nil {
			return
		}
		obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
		obj.Call("org.freedesktop.Notifications.Notify", 0,
			"ZITI-GUI", uint32(0), "", summary, body,
			[]string{}, map[string]dbus.Variant{}, int32(6000))
	}()
}
