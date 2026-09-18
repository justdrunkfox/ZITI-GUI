package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fyne.io/systray"
)

// --------------------------------------------------------------- жизненный цикл

func (a *app) onTrayReady() {
	systray.SetIcon(iconPNG(SvcInactive, false))
	systray.SetTooltip("ZITI-GUI")
	a.quit = systray.Quit
	go a.pollLoop()
	a.openWindow()
}

func (a *app) pollLoop() {
	a.mu.Lock()
	cur := a.cur
	a.mu.Unlock()
	a.ver = cur.key()
	a.buildMenu(cur)

	a.poll()

	t := time.NewTicker(time.Duration(a.cfg.PollSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-a.updateCh: // действие просит обновиться побыстрее
		}
		a.poll()
		a.rebuildIfChanged()
		invalidateWindow()
	}
}

func (a *app) rebuildIfChanged() {
	a.mu.Lock()
	cur := a.cur
	armed := make([]string, 0, len(a.delArmed))
	for n := range a.delArmed {
		armed = append(armed, n)
	}
	a.mu.Unlock()
	sort.Strings(armed)

	sig := cur.key() + "|" + strings.Join(armed, ",")
	if sig == a.ver {
		return
	}
	a.ver = sig
	a.buildMenu(cur)
}

// ---------------------------------------------------------------------- меню

func onClick(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			fn()
		}
	}()
}

func (a *app) buildMenu(st Status) {
	systray.SetIcon(iconPNG(st.Svc, st.IpcOK))
	systray.SetTooltip("Ziti Tunnel — " + a.statusWord(st))
	systray.ResetMenu()

	info := systray.AddMenuItem(a.statusText(st), "Текущее состояние туннелера")
	info.Disable()
	mOpen := systray.AddMenuItem("Открыть окно", "Показать главное окно ZITI-GUI")
	onClick(mOpen, a.openWindow)
	systray.AddSeparator()

	miStart := systray.AddMenuItem("Запустить сервис", "systemctl start "+a.cfg.Service)
	miStop := systray.AddMenuItem("Остановить сервис", "systemctl stop "+a.cfg.Service)
	miRestart := systray.AddMenuItem("Перезапустить сервис", "systemctl restart "+a.cfg.Service)
	if st.Svc == SvcActive || st.Svc == SvcActivating {
		miStart.Disable()
	}
	if st.Svc == SvcInactive || st.Svc == SvcFailed || st.Svc == SvcUnknown {
		miStop.Disable()
		miRestart.Disable()
	}
	onClick(miStart, a.svcStart)
	onClick(miStop, a.svcStop)
	onClick(miRestart, a.svcRestart)

	systray.AddSeparator()
	a.buildIdentitiesMenu(st)

	systray.AddSeparator()
	mEnroll := systray.AddMenuItem("Зарегистрировать JWT…", "Выбрать JWT-файл и выполнить enrollment")
	onClick(mEnroll, func() { a.once("enroll", a.enrollFlow) })

	if a.needSetup(st) {
		mSetup := systray.AddMenuItem("⚙ Настроить доступ (пароль один раз)",
			"Добавит вас в группу ziti, откроет IPC и разрешит управлять сервисом без пароля")
		onClick(mSetup, a.runSetup)
	}

	mDiag := systray.AddMenuItem("Диагностика", "")
	l1 := mDiag.AddSubMenuItem("ziti-edge-tunnel: "+a.zitiVersion(), "")
	l1.Disable()
	l2 := mDiag.AddSubMenuItem("Сервис: "+a.diagSvc(st), "")
	l2.Disable()
	ipcLine := "IPC: недоступен"
	if st.IpcOK {
		ipcLine = "IPC: доступен"
	} else if st.IpcErr != "" {
		ipcLine = "IPC: " + st.IpcErr
	}
	l3 := mDiag.AddSubMenuItem(ipcLine, "")
	l3.Disable()
	acc := "нет доступа"
	if _, err := os.ReadDir(a.cfg.IdentityDir); err == nil {
		acc = "доступен"
	}
	l4 := mDiag.AddSubMenuItem("Каталог: "+a.cfg.IdentityDir+" ("+acc+")", "")
	l4.Disable()
	mJournal := mDiag.AddSubMenuItem("Журнал сервиса (300 строк)", "journalctl через pkexec")
	onClick(mJournal, a.openJournal)
	mDump := mDiag.AddSubMenuItem("Сохранить dump → /tmp/ziti-gui-dump.json", "")
	onClick(mDump, a.saveDump)
	mDir := mDiag.AddSubMenuItem("Открыть каталог идентичностей", "")
	onClick(mDir, func() { openInViewer(a.cfg.IdentityDir) })

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Закрыть GUI (туннелер продолжит работать)")
	onClick(mQuit, a.quitApp)
}

// quitApp завершает GUI; под user-юнитом — останавливает юнит, чтобы
// Restart=always не поднял процесс заново.
func (a *app) quitApp() {
	if os.Getenv("INVOCATION_ID") != "" {
		_, _ = runCmd(context.Background(), "systemctl", "--user", "stop", "ziti-gui.service")
	}
	if a.quit != nil {
		a.quit()
	}
}

func (a *app) buildIdentitiesMenu(st Status) {
	mIdents := systray.AddMenuItem("Идентичности", "Статус, вкл/выкл, enrollment")
	if len(st.Idents) == 0 {
		line := "нет идентичностей"
		if st.Svc == SvcActive && st.IpcErr != "" {
			line = "нет доступа к статусам — см. «Настроить доступ»"
		}
		none := mIdents.AddSubMenuItem(line, "")
		none.Disable()
		return
	}
	toggleBlocked := st.Svc == SvcActive && !st.IpcOK
	for _, id := range st.Idents {
		id := id
		mark := "⚪"
		if id.EnabledKnown {
			if id.Enabled {
				mark = "🟢"
			} else {
				mark = "⚫"
			}
		}
		if id.NeedsAttention() {
			mark = "⚠"
		}
		title := fmt.Sprintf("%s %s · сервисов: %d", mark, id.Name, len(id.Services))
		if !id.EnabledKnown {
			title = fmt.Sprintf("%s %s (файл)", mark, id.Name)
		}
		sub := mIdents.AddSubMenuItem(title, a.identDetail(id))

		detail := sub.AddSubMenuItem(a.identDetail(id), "")
		detail.Disable()

		tog := sub.AddSubMenuItemCheckbox(
			boolWord(id.Enabled),
			"Переключить идентичность (сохраняется между стартами)",
			id.Enabled)
		if toggleBlocked {
			tog.Disable()
		}
		onClick(tog, func() { a.toggleIdent(id.Name, !id.Enabled) })

		ref := sub.AddSubMenuItem("Обновить с контроллера", "ziti-edge-tunnel refresh")
		if !st.IpcOK {
			ref.Disable()
		}
		onClick(ref, func() { a.refreshIdent(id.Name) })

		sub.AddSeparator()
		delTitle := "Удалить…"
		if a.delArmed[id.Name] {
			delTitle = "⚠ Точно удалить «" + id.Name + "»?"
		}
		del := sub.AddSubMenuItem(delTitle, "Удалить из туннелера и стереть файл")
		onClick(del, func() { a.deleteIdent(id) })
	}
}

func boolWord(b bool) string {
	if b {
		return "Включена (нажмите, чтобы выключить)"
	}
	return "Выключена (нажмите, чтобы включить)"
}

// -------------------------------------------------------------- текстовки

func (a *app) statusWord(st Status) string {
	switch st.Svc {
	case SvcActive:
		if st.IpcOK {
			return "запущен"
		}
		return "запущен (IPC недоступен)"
	case SvcActivating:
		return "запускается…"
	case SvcInactive:
		return "остановлен"
	case SvcFailed:
		return "сбой"
	}
	return "статус неизвестен"
}

func (a *app) statusText(st Status) string {
	switch st.Svc {
	case SvcActive:
		if st.IpcOK {
			n := 0
			for _, id := range st.Idents {
				if id.Enabled {
					n += len(id.Services)
				}
			}
			return fmt.Sprintf("● Запущен · PID %s · сервисов Ziti: %d", st.PID, n)
		}
		return "● Запущен, но статусы недоступны (см. «Настроить доступ»)"
	case SvcActivating:
		return "● Запускается…"
	case SvcInactive:
		return "○ Остановлен"
	case SvcFailed:
		return "✗ Сбой сервиса"
	}
	return "? Статус неизвестен"
}

func (a *app) diagSvc(st Status) string {
	s := fmt.Sprintf("%s — %s", a.cfg.Service, st.Svc)
	if st.PID != "" && st.PID != "0" {
		s += " (PID " + st.PID + ")"
	}
	return s
}

func (a *app) identDetail(id Ident) string {
	var parts []string
	if id.EnabledKnown {
		if id.Enabled {
			parts = append(parts, "вкл")
		} else {
			parts = append(parts, "выкл")
		}
	} else {
		parts = append(parts, "не в туннелере")
	}
	if id.AuthState != "" && id.AuthState != "FullyAuthenticated" {
		parts = append(parts, "auth: "+id.AuthState)
	}
	if id.TOTPRequired && !id.TOTPEnrolled {
		parts = append(parts, "⚠ требуется MFA")
	}
	if len(id.IPs) > 0 {
		parts = append(parts, strings.Join(id.IPs, ", "))
	}
	const maxSvc = 5
	if len(id.Services) > 0 {
		labels := make([]string, 0, len(id.Services))
		for _, s := range id.Services {
			labels = append(labels, s.label())
		}
		if len(labels) > maxSvc {
			labels = append(labels[:maxSvc],
				fmt.Sprintf("+%d ещё", len(id.Services)-maxSvc))
		}
		parts = append(parts, strings.Join(labels, ", "))
	}
	return strings.Join(parts, " · ")
}

// ------------------------------------------------------------ enrollment

func (a *app) enrollFlow() {
	log.Printf("enroll: старт")
	path, err := pickFileForEnroll("Выберите enrollment JWT")
	if err != nil {
		log.Printf("enroll: диалог: %v", err)
		notify("Ziti: диалог не открылся", err.Error())
		return
	}
	if path == "" {
		log.Printf("enroll: отменено пользователем")
		return
	}
	name := sanitizeName(path)
	log.Printf("enroll: файл %s, имя %q", path, name)

	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()
	ctx := context.Background()

	// сервис работает и IPC доступен — add регистрирует и сразу подгружает.
	// ВАЖНО: у add флаг -j ждёт СОДЕРЖИМОЕ токена (не путь), и CLI может
	// вернуть Success:false с кодом 0 — проверяем ответ.
	if st.IpcOK && st.Svc == SvcActive {
		jwt, rerr := os.ReadFile(path)
		if rerr != nil {
			log.Printf("enroll: не прочитал jwt: %v", rerr)
			notify("Ziti: не удалось прочитать JWT", rerr.Error())
			return
		}
		out, aerr := a.zitiClient(ctx, 90*time.Second, "add", "-i", name, "-j", string(jwt))
		if msg := zitiIPCError(out, aerr); msg == "" {
			log.Printf("enroll: add ok")
			notify("Ziti", "Идентичность «"+name+"» зарегистрирована и загружена")
			a.requestUpdate()
			return
		} else {
			log.Printf("enroll: add не сработал (%s), вывел: %s", msg, firstLine(out, nil))
		}
	}

	// общий путь: enroll во временный файл, затем укладка в каталог сервиса
	tmp := filepath.Join(os.TempDir(), "ziti-gui-enroll-"+name+".json")
	defer os.Remove(tmp)
	out, eerr := a.zitiClient(ctx, 90*time.Second, "enroll", "-j", path, "-i", tmp, "-n", name)
	if msg := zitiIPCError(out, eerr); msg != "" {
		log.Printf("enroll: enroll FAIL %s", msg)
		notify("Ziti: регистрация не удалась", msg)
		return
	}
	log.Printf("enroll: enroll ok, укладываю файл")

	su := a.svcUser()
	target := filepath.Join(a.cfg.IdentityDir, name+".json")
	script := fmt.Sprintf("install -m 660 -o %q -g %q %q %q && echo INST_OK", su, su, tmp, target)
	pout, perr := runCmd(ctx, "pkexec", "/bin/sh", "-c", script, "sh")
	if perr != nil || !strings.Contains(pout, "INST_OK") {
		log.Printf("enroll: install FAIL %s %v", firstLine(pout, nil), perr)
		notify("Ziti: зарегистрирована, но файл не уложен",
			friendlyErr(pout, perr)+". Файл: "+tmp)
		return
	}

	if st.Svc == SvcActive {
		log.Printf("enroll: перезапускаю сервис для загрузки")
		notify("Ziti", "«"+name+"» зарегистрирована — перезапускаю сервис")
		a.svcRestart()
	} else {
		log.Printf("enroll: готово, сервис остановлен")
		notify("Ziti", "«"+name+"» зарегистрирована. Запустите сервис")
	}
	a.requestUpdate()
}
