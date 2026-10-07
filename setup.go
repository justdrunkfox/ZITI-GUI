package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Разовая настройка доступа: добавляет пользователя в группу ziti,
// открывает ACL на IPC-сокет текущей сессии и ставит polkit-правило,
// разрешающее управлять сервисом без пароля. Выполняется одним pkexec.
const setupScript = `set -e
user="$1"; svc="$2"; idir="$3"
echo "user=$user svc=$svc idir=$idir"
id -u ziti >/dev/null 2>&1 || { echo "ERR: нет пользователя ziti"; exit 1; }
usermod -aG ziti "$user" 2>/dev/null || echo "WARN: usermod не сработал (возможно, уже в группе)"
if command -v setfacl >/dev/null 2>&1; then
    setfacl -m "u:${user}:rx" /tmp/.ziti 2>/dev/null || true
    setfacl -m "u:${user}:rx" "$idir" 2>/dev/null || true
    for s in /tmp/.ziti/*.sock; do
        [ -e "$s" ] && setfacl -m "u:${user}:rw" "$s" 2>/dev/null || true
    done
else
    echo "WARN: setfacl не установлен; доступ появится после перелогина (группа ziti)"
fi
mkdir -p /etc/polkit-1/rules.d
cat > /etc/polkit-1/rules.d/50-ziti-gui.rules <<RULE
// Сгенерировано ziti-gui: управлять ${svc} без запроса пароля
polkit.addRule(function(action, subject) {
    if (action.id == "org.freedesktop.systemd1.manage-units" &&
        subject.user == "${user}" &&
        action.lookup("unit") == "${svc}") {
        return polkit.Result.YES;
    }
});
RULE
chmod 644 /etc/polkit-1/rules.d/50-ziti-gui.rules
echo "SETUP_OK"`

func (a *app) needSetup(st Status) bool {
	return st.Svc == SvcActive && !st.IpcOK
}

// allowedIdentityDir — каталог идентичностей, который допустимо отдавать
// root-скрипту настройки: пакетный путь по умолчанию или что-то внутри $HOME.
func (a *app) allowedIdentityDir() bool {
	if dir, err := filepath.Abs(a.cfg.IdentityDir); err == nil {
		if dir == "/opt/openziti/etc/identities" {
			return true
		}
		if home, herr := os.UserHomeDir(); herr == nil &&
			strings.HasPrefix(dir, home+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (a *app) runSetup() {
	a.once("setup", func() {
		if !a.allowedIdentityDir() {
			notify("Ziti: настройка отменена",
				"Подозрительный identity_dir в конфиге: "+a.cfg.IdentityDir)
			return
		}
		me, err := user.Current()
		if err != nil {
			notify("Ziti: настройка не удалась", err.Error())
			return
		}
		out, err := runCmd(context.Background(), "pkexec",
			"/bin/sh", "-c", setupScript, "sh", me.Username, a.cfg.Service, a.cfg.IdentityDir)
		if isCancelled(err) {
			return // пользователь отменил запрос пароля
		}
		if err != nil {
			notify("Ziti: настройка прервана", friendlyErr(out, err))
			return
		}
		if strings.Contains(out, "SETUP_OK") {
			msg := "Готово: сервис управляется без пароля"
			if strings.Contains(out, "WARN") {
				msg += ". Если IPC недоступен — перелогиньтесь, чтобы применилась группа ziti"
			}
			notify("Ziti: доступ настроен", msg)
		} else {
			notify("Ziti: настройка завершилась странно", firstLine(out, nil))
		}
		a.requestUpdate()
	})
}

func isCancelled(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "dismissed") || strings.Contains(s, "not authorized") ||
		strings.Contains(s, "cancelled") || strings.Contains(s, "canceled")
}

// journalTail сохраняет хвост журнала сервиса в /tmp и открывает его.
func (a *app) openJournal() {
	a.once("journal", func() {
		target := "/tmp/ziti-gui-journal.txt"
		script := fmt.Sprintf(
			"journalctl -u %q -n 300 --no-pager -o short-precise > %q && chmod 666 %q && echo J_OK",
			a.cfg.Service, target, target)
		out, err := runCmd(context.Background(), "pkexec", "/bin/sh", "-c", script, "sh")
		if err != nil || !strings.Contains(out, "J_OK") {
			notify("Ziti: журнал недоступен", friendlyErr(out, err))
			return
		}
		openInViewer(target)
	})
}

// saveDump выгружает текущее состояние туннелера в файл.
func (a *app) saveDump() {
	a.once("dump", func() {
		out, err := a.zitiClient(context.Background(), 10*time.Second, "dump")
		target := "/tmp/ziti-gui-dump.json"
		if err != nil {
			notify("Ziti: dump недоступен", firstLine(out, err))
			return
		}
		if werr := os.WriteFile(target, []byte(out), 0o644); werr != nil {
			notify("Ziti: не удалось сохранить", werr.Error())
			return
		}
		notify("Ziti", "Состояние сохранено в "+target)
	})
}

func openInViewer(path string) {
	runCmd(context.Background(), "xdg-open", path)
}
