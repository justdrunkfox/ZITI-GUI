package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gioapp "gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// ------------------------------------------------------------------- палитра

var (
	colBg      = color.NRGBA{R: 0x13, G: 0x17, B: 0x21, A: 0xFF}
	colCard    = color.NRGBA{R: 0x1C, G: 0x22, B: 0x2F, A: 0xFF}
	colCardSel = color.NRGBA{R: 0x27, G: 0x30, B: 0x42, A: 0xFF}
	colRow     = color.NRGBA{R: 0x19, G: 0x1F, B: 0x2A, A: 0xFF}
	colFg      = color.NRGBA{R: 0xE6, G: 0xEA, B: 0xF2, A: 0xFF}
	colFgDim   = color.NRGBA{R: 0x94, G: 0x9E, B: 0xAF, A: 0xFF}
	colAccent  = color.NRGBA{R: 0x5B, G: 0x9B, B: 0xFF, A: 0xFF}
	colGreen   = color.NRGBA{R: 0x35, G: 0xC7, B: 0x5A, A: 0xFF}
	colYellow  = color.NRGBA{R: 0xFF, G: 0xB0, B: 0x20, A: 0xFF}
	colRed     = color.NRGBA{R: 0xFF, G: 0x45, B: 0x3A, A: 0xFF}
	colGray    = color.NRGBA{R: 0x8E, G: 0x8E, B: 0x93, A: 0xFF}
	colOrange  = color.NRGBA{R: 0xFF, G: 0x9F, B: 0x0A, A: 0xFF}
)

func statusColor(st Status) color.NRGBA {
	switch st.Svc {
	case SvcActive:
		if st.IpcOK {
			return colGreen
		}
		return colOrange
	case SvcActivating:
		return colYellow
	case SvcInactive:
		return colGray
	case SvcFailed:
		return colRed
	}
	return colOrange
}

// --------------------------------------------------------------- состояние UI

type windowState struct {
	idList  widget.List
	svcList widget.List
	// selected — имя выбранной идентичности
	selected  string
	cards     map[string]*widget.Clickable
	switches  map[string]*widget.Bool
	svcClicks map[string]*widget.Clickable
	// copiedKey/copiedAt — что и когда скопировали (для отметки «скопировано»)
	copiedKey                                                      string
	copiedAt                                                       time.Time
	btnStart, btnStop, btnRestart, btnEnroll, btnSetup, btnJournal widget.Clickable
	btnRefresh, btnDelete                                          widget.Clickable
	// invalidate просит окно перерисоваться (например, после клика по карточке)
	invalidate func()
}

func newWindowState() *windowState {
	return &windowState{
		idList:    widget.List{List: layout.List{Axis: layout.Vertical}},
		svcList:   widget.List{List: layout.List{Axis: layout.Vertical}},
		cards:     map[string]*widget.Clickable{},
		switches:  map[string]*widget.Bool{},
		svcClicks: map[string]*widget.Clickable{},
	}
}

func (ui *windowState) cardFor(id Ident) *widget.Clickable {
	c, ok := ui.cards[id.Name]
	if !ok {
		c = new(widget.Clickable)
		ui.cards[id.Name] = c
	}
	return c
}

func (ui *windowState) clickFor(key string) *widget.Clickable {
	c, ok := ui.svcClicks[key]
	if !ok {
		c = new(widget.Clickable)
		ui.svcClicks[key] = c
	}
	return c
}

// markCopied запоминает факт копирования: сразу перерисовываем (отметка
// «скопировано»), а через 2с — ещё раз, чтобы её убрать.
func (ui *windowState) markCopied(key string) {
	ui.copiedKey = key
	ui.copiedAt = time.Now()
	invalidateWindow()
	time.AfterFunc(2*time.Second, invalidateWindow)
}

func (ui *windowState) isFresh(key string) bool {
	return ui.copiedKey == key && time.Since(ui.copiedAt) < 2*time.Second
}

// copyText кладёт текст в буфер обмена (gio + wl-copy как страховка).
// Всё выполняется в фоне: fork/exec в UI-потоке останавливает мир и даёт
// ощутимый лаг между кликом и отметкой «скопировано».
func copyText(gtx layout.Context, text string) {
	gtx.Execute(clipboard.WriteCmd{Type: "application/text",
		Data: io.NopCloser(strings.NewReader(text))})
	go func() {
		if os.Getenv("WAYLAND_DISPLAY") == "" {
			return // X11: буфер уже записан средствами gio
		}
		if wl, err := exec.LookPath("wl-copy"); err == nil {
			_ = exec.Command(wl, text).Start()
		}
	}()
}

func (ui *windowState) switchFor(id Ident) *widget.Bool {
	sw, ok := ui.switches[id.Name]
	if !ok {
		sw = &widget.Bool{Value: id.Enabled}
		ui.switches[id.Name] = sw
	}
	return sw
}

// -------------------------------------------------------------------- окно

var (
	winMu   sync.Mutex
	winRef  *gioapp.Window
	winOpen atomic.Bool
)

// openWindow открывает окно (или игнорирует вызов, если оно уже открыто).
// Если Wayland-дисплей ещё не готов (юнит стартует вместе с сеансом) —
// ждёт сокет и повторяет попытки.
func (a *app) openWindow() {
	if !winOpen.CompareAndSwap(false, true) {
		focusExistingWindow()
		return
	}
	go func() {
		defer winOpen.Store(false)
		waitWayland(2 * time.Minute)
		for attempt := 1; ; attempt++ {
			// env юнита может не содержать WAYLAND_DISPLAY (стартовал раньше
			// сеанса) — находим живой сокет сами
			if name := findWaylandSocket(); name != "" && name != os.Getenv("WAYLAND_DISPLAY") {
				log.Printf("window: использую wayland-сокет %s", name)
				os.Setenv("WAYLAND_DISPLAY", name)
			}
			w := new(gioapp.Window)
			w.Option(gioapp.Title("ZITI-GUI"), gioapp.Size(unit.Dp(1000), unit.Dp(660)))
			winMu.Lock()
			winRef = w
			winMu.Unlock()
			err := a.runWindow(w)
			winMu.Lock()
			winRef = nil
			winMu.Unlock()
			if err == nil {
				log.Printf("window closed by user")
				return // штатное закрытие пользователем
			}
			if attempt >= 12 {
				log.Printf("window: giving up after %d attempts: %v", attempt, err)
				notify("ZITI-GUI: окно не открылось", err.Error())
				return
			}
			log.Printf("window attempt %d failed: %v — retry in 5s", attempt, err)
			time.Sleep(5 * time.Second)
		}
	}()
}

// focusExistingWindow поднимает уже открытое окно: ActionRaise (X11/Win/macOS)
// плюс фолбэк через IPC niri (драйвер Wayland в gio Raise не реализует).
func focusExistingWindow() {
	winMu.Lock()
	w := winRef
	winMu.Unlock()
	if w != nil {
		w.Perform(system.ActionRaise)
	}
	if os.Getenv("NIRI_SOCKET") == "" {
		return
	}
	out, err := exec.Command("niri", "msg", "--json", "windows").Output()
	if err != nil {
		return
	}
	var wins []struct {
		ID    int    `json:"id"`
		AppID string `json:"app_id"`
		Title string `json:"title"`
	}
	if json.Unmarshal(out, &wins) != nil {
		return
	}
	for _, win := range wins {
		if strings.EqualFold(win.AppID, "ziti-gui") || strings.EqualFold(win.Title, "ZITI-GUI") {
			if win.ID != 0 {
				_ = exec.Command("niri", "msg", "action",
					"focus-window", "--id", strconv.Itoa(win.ID)).Run()
			}
			return
		}
	}
}

// findWaylandSocket возвращает имя живого wayland-сокета: сперва
// $WAYLAND_DISPLAY, затем любой wayland-* в XDG_RUNTIME_DIR (новейший).
func findWaylandSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	if name := os.Getenv("WAYLAND_DISPLAY"); name != "" {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return name
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestMod := "", time.Time{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "wayland-") || strings.HasSuffix(n, ".lock") {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = n, fi.ModTime()
		}
	}
	return best
}

// waitWayland ждёт появления wayland-сокета в XDG_RUNTIME_DIR.
// Если Wayland нет, но есть X11 — не ждём: gio откроет окно через X11.
func waitWayland(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if findWaylandSocket() != "" {
			return
		}
		if os.Getenv("DISPLAY") != "" {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("wayland-сокет не появился за %v", timeout)
			return
		}
		time.Sleep(time.Second)
	}
}

func invalidateWindow() {
	winMu.Lock()
	w := winRef
	winMu.Unlock()
	if w != nil {
		w.Invalidate()
	}
}

func (a *app) runWindow(w *gioapp.Window) error {
	th := material.NewTheme()
	th.Bg, th.Fg = colBg, colFg
	th.ContrastBg, th.ContrastFg = colAccent, colFg
	ui := newWindowState()
	ui.invalidate = w.Invalidate
	var ops op.Ops
	for {
		e := w.Event()
		switch e := e.(type) {
		case gioapp.DestroyEvent:
			return e.Err
		case gioapp.FrameEvent:
			gtx := gioapp.NewContext(&ops, e)
			a.layoutRoot(gtx, th, ui)
			e.Frame(gtx.Ops)
		}
	}
}

// ------------------------------------------------------------------- раскладка

func (a *app) layoutRoot(gtx layout.Context, th *material.Theme, ui *windowState) layout.Dimensions {
	paint.FillShape(gtx.Ops, colBg, clip.Rect{Max: gtx.Constraints.Max}.Op())
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.layoutHeader(gtx, th, ui)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.layoutSetupBanner(gtx, th, ui)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return a.layoutBody(gtx, th, ui)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.layoutFooter(gtx, th, ui)
		}),
	)
}

func (a *app) layoutHeader(gtx layout.Context, th *material.Theme, ui *windowState) layout.Dimensions {
	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()

	return layout.Inset{
		Top: unit.Dp(14), Bottom: unit.Dp(10),
		Left: unit.Dp(18), Right: unit.Dp(18),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return dot(gtx, statusColor(st), gtx.Dp(14))
					}),
					layout.Rigid(spacer(gtx.Dp(10))),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, strings.TrimLeft(a.statusText(st), "\u25cf\u25cb\u2717? "))
						lbl.TextSize = unit.Sp(17)
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return a.headerButtons(gtx, th, ui, st)
				})
			}),
		)
	})
}

func (a *app) headerButtons(gtx layout.Context, th *material.Theme, ui *windowState, st Status) layout.Dimensions {
	active := st.Svc == SvcActive
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return flatButton(gtx, th, &ui.btnStart, "Запустить", !active, colGreen,
				func() { go a.svcStart() })
		}),
		layout.Rigid(spacer(gtx.Dp(8))),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return flatButton(gtx, th, &ui.btnStop, "Остановить", active, colRed,
				func() { go a.svcStop() })
		}),
		layout.Rigid(spacer(gtx.Dp(8))),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return flatButton(gtx, th, &ui.btnRestart, "Перезапустить", active, colAccent,
				func() { go a.svcRestart() })
		}),
		layout.Rigid(spacer(gtx.Dp(8))),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return flatButton(gtx, th, &ui.btnEnroll, "Зарегистрировать JWT…", true, colAccent,
				func() { a.once("enroll", a.enrollFlow) })
		}),
	)
}

func (a *app) layoutSetupBanner(gtx layout.Context, th *material.Theme, ui *windowState) layout.Dimensions {
	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()
	if !a.needSetup(st) {
		return layout.Dimensions{}
	}
	for ui.btnSetup.Clicked(gtx) {
		a.runSetup()
	}
	return layout.Inset{
		Left: unit.Dp(18), Right: unit.Dp(18), Bottom: unit.Dp(6),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillBehind(gtx, color.NRGBA{R: 0x3A, G: 0x2B, B: 0x10, A: 0xFF}, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "Нет доступа к IPC туннелера — статусы идентичностей недоступны")
						lbl.Color = colOrange
						return lbl.Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Dimensions{}
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &ui.btnSetup, "⚙ Настроить доступ")
						btn.Background = colOrange
						btn.Color = color.NRGBA{A: 0xFF}
						return btn.Layout(gtx)
					}),
				)
			})
		})
	})
}

func (a *app) layoutBody(gtx layout.Context, th *material.Theme, ui *windowState) layout.Dimensions {
	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()

	// дефолтный выбор — первая идентичность; выбранная могла исчезнуть
	if len(st.Idents) > 0 {
		found := false
		for _, id := range st.Idents {
			if id.Name == ui.selected {
				found = true
				break
			}
		}
		if !found {
			ui.selected = st.Idents[0].Name
		}
	}

	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Dp(330)
			gtx.Constraints.Max.X = gtx.Dp(330)
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			return a.layoutIdentities(gtx, th, ui, st)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			paint.FillShape(gtx.Ops, colCard, clip.Rect{Max: image.Pt(1, gtx.Constraints.Max.Y)}.Op())
			return layout.Dimensions{Size: image.Pt(1, gtx.Constraints.Max.Y)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			return a.layoutServices(gtx, th, ui, st)
		}),
	)
}

func (a *app) layoutIdentities(gtx layout.Context, th *material.Theme, ui *windowState, st Status) layout.Dimensions {
	return layout.Inset{
		Top: unit.Dp(10), Left: unit.Dp(12), Right: unit.Dp(12), Bottom: unit.Dp(8),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Caption(th, "ИДЕНТИЧНОСТИ")
					lbl.Color = colFgDim
					return lbl.Layout(gtx)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(st.Idents) == 0 {
					return centeredText(gtx, th, emptyIdentsHint(st))
				}
				return material.List(th, &ui.idList).Layout(gtx, len(st.Idents), func(gtx layout.Context, i int) layout.Dimensions {
					return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return a.identityCard(gtx, th, ui, st.Idents[i])
					})
				})
			}),
		)
	})
}

func emptyIdentsHint(st Status) string {
	if st.Svc == SvcActive && st.IpcErr != "" {
		return "Нет доступа к статусам (IPC).\nНажмите «⚙ Настроить доступ»"
	}
	return "Нет идентичностей.\nНажмите «Зарегистрировать JWT…»"
}

func (a *app) identityCard(gtx layout.Context, th *material.Theme, ui *windowState, id Ident) layout.Dimensions {
	sw := ui.switchFor(id)
	if changed := sw.Update(gtx); changed {
		go a.toggleIdent(id.Name, sw.Value)
	} else if sw.Value != id.Enabled {
		sw.Value = id.Enabled
	}
	card := ui.cardFor(id)
	for card.Clicked(gtx) {
		if ui.selected != id.Name {
			ui.selected = id.Name
			if ui.invalidate != nil {
				ui.invalidate()
			}
		}
	}
	selected := ui.selected == id.Name

	bg := colCard
	if selected {
		bg = colCardSel
	}
	bar := colGray
	if id.NeedsAttention() {
		bar = colOrange
	} else if id.EnabledKnown && id.Enabled {
		bar = colGreen
	}
	// card.Layout рисует контент и регистрирует кликабельную зону на всю карточку
	return card.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return fillBehindBar(gtx, bg, bar, gtx.Dp(4), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{
				Top: unit.Dp(10), Bottom: unit.Dp(10), Left: unit.Dp(14), Right: unit.Dp(10),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, id.Name)
								lbl.TextSize = unit.Sp(16)
								if id.EnabledKnown && !id.Enabled {
									lbl.Color = colFgDim
								}
								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return material.Switch(th, sw, "").Layout(gtx)
							}),
						)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, idSubtitle(id))
							lbl.Color = colFgDim
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		})
	})
}

func idSubtitle(id Ident) string {
	if !id.EnabledKnown {
		return "не загружена — нет связи с контроллером?"
	}
	s := "вкл"
	if !id.Enabled {
		s = "выкл"
	}
	if len(id.Services) > 0 {
		s += " · сервисов: " + strconv.Itoa(len(id.Services))
	}
	if id.TOTPRequired && !id.TOTPEnrolled {
		s += " · ⚠ MFA"
	} else if id.AuthState != "" && id.AuthState != "FullyAuthenticated" {
		s += " · auth: " + id.AuthState
	}
	return s
}

func (a *app) layoutServices(gtx layout.Context, th *material.Theme, ui *windowState, st Status) layout.Dimensions {
	var sel *Ident
	for i := range st.Idents {
		if st.Idents[i].Name == ui.selected {
			sel = &st.Idents[i]
			break
		}
	}
	return layout.Inset{
		Top: unit.Dp(10), Left: unit.Dp(16), Right: unit.Dp(16), Bottom: unit.Dp(8),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							title := "СЕРВИСЫ"
							if sel != nil {
								title = "СЕРВИСЫ · " + sel.Name
							}
							lbl := material.Caption(th, title)
							lbl.Color = colFgDim
							return lbl.Layout(gtx)
						}),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Dimensions{}
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return flatButton(gtx, th, &ui.btnRefresh, "Обновить", st.IpcOK, colAccent,
								func() {
									if sel != nil {
										a.refreshIdent(sel.Name)
									}
								})
						}),
						layout.Rigid(spacer(gtx.Dp(4))),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							delTxt := "Удалить…"
							a.mu.Lock()
							armed := a.delArmed[ui.selected]
							a.mu.Unlock()
							if sel != nil && armed {
								delTxt = "⚠ Точно удалить «" + sel.Name + "»?"
							}
							return flatButton(gtx, th, &ui.btnDelete, delTxt, sel != nil, colRed,
								func() {
									if sel != nil {
										a.deleteIdent(*sel)
									}
								})
						}),
					)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if sel == nil {
					return centeredText(gtx, th, "Выберите идентичность слева")
				}
				if !sel.EnabledKnown {
					return centeredText(gtx, th, "Идентичность не загружена в туннелер:\nконтроллер недоступен или не прошла аутентификация.\nНажмите «Обновить», чтобы повторить попытку")
				}
				if len(sel.Services) == 0 {
					return centeredText(gtx, th, "Нет сервисов")
				}
				return material.List(th, &ui.svcList).Layout(gtx, len(sel.Services), func(gtx layout.Context, i int) layout.Dimensions {
					return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return a.serviceRow(gtx, th, ui, sel.Services[i])
					})
				})
			}),
		)
	})
}

func (a *app) serviceRow(gtx layout.Context, th *material.Theme, ui *windowState, svc ServiceInfo) layout.Dimensions {
	mode, modeCol := "dial", colGreen
	if svc.Bind && !svc.Dial {
		mode, modeCol = "bind", colYellow
	}
	addrs := svc.Addrs
	if len(addrs) == 0 {
		if svc.Addr != "" {
			addrs = []string{svc.Addr}
		} else {
			addrs = []string{"—"}
		}
	}
	const maxAddrs = 5
	nameKey := "name:" + svc.Name
	hint := ui.isFresh("hint:" + nameKey)

	return fillBehind(gtx, colRow, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Inset{
			Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							nc := ui.clickFor(nameKey)
							for nc.Clicked(gtx) {
								if len(addrs) == 1 && addrs[0] != "—" {
									copyText(gtx, addrs[0])
									ui.markCopied(svc.Name + "|" + addrs[0])
								} else if len(addrs) > 1 {
									ui.markCopied("hint:" + nameKey)
								}
							}
							return nc.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, svc.Name)
								lbl.TextSize = unit.Sp(15)
								return lbl.Layout(gtx)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, mode)
							lbl.Color = modeCol
							return lbl.Layout(gtx)
						}),
					)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if hint {
						return layout.Inset{Top: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, "адресов несколько — кликни нужный ниже, он попадёт в буфер")
							lbl.Color = colFgDim
							return lbl.Layout(gtx)
						})
					}
					return layout.Inset{Top: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return a.addrLines(gtx, th, ui, svc, maxAddrs)
					})
				}),
			)
		})
	})
}

// addrLines выводит intercept-адреса отдельными кликабельными строками:
// клик копирует имя сервера в буфер обмена.
func (a *app) addrLines(gtx layout.Context, th *material.Theme, ui *windowState, svc ServiceInfo, max int) layout.Dimensions {
	addrs := svc.Addrs
	if len(addrs) == 0 {
		if svc.Addr != "" {
			addrs = []string{svc.Addr}
		} else {
			addrs = []string{"—"}
		}
	}
	var children []layout.FlexChild
	for i := 0; i < len(addrs) && i < max; i++ {
		host := addrs[i]
		key := svc.Name + "|" + host
		c := ui.clickFor(key)
		copied := ui.isFresh(key)
		for c.Clicked(gtx) {
			if host != "—" {
				t0 := time.Now()
				copyText(gtx, host)
				ui.markCopied(key)
				log.Printf("copy %q обработан за %v", host, time.Since(t0))
			}
		}
		text := host
		if host != "—" && svc.Port != "" {
			text += ":" + svc.Port
		}
		if copied {
			text = "✓ скопировано: " + host
		}
		line := text
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, line)
				if copied {
					lbl.Color = colGreen
				} else {
					lbl.Color = colAccent
				}
				return lbl.Layout(gtx)
			})
		}))
	}
	if extra := len(addrs) - max; extra > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Caption(th, fmt.Sprintf("+%d ещё", extra))
			lbl.Color = colFgDim
			return lbl.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (a *app) layoutFooter(gtx layout.Context, th *material.Theme, ui *windowState) layout.Dimensions {
	a.mu.Lock()
	st := a.cur
	a.mu.Unlock()

	for ui.btnJournal.Clicked(gtx) {
		a.openJournal()
	}

	line := a.zitiVersion()
	if st.IpcErr != "" {
		line += " · IPC: " + st.IpcErr
	} else if st.IpcOK {
		line += " · IPC: доступен"
	}

	return layout.Inset{
		Top: unit.Dp(6), Bottom: unit.Dp(8), Left: unit.Dp(18), Right: unit.Dp(18),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, line)
				lbl.Color = colFgDim
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flatButton(gtx, th, &ui.btnJournal, "Журнал сервиса", true, colAccent,
					a.openJournal)
			}),
		)
	})
}

// -------------------------------------------------------------- примитивы

func spacer(w int) func(layout.Context) layout.Dimensions {
	return func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = w
		return layout.Dimensions{Size: image.Pt(w, 0)}
	}
}

// flatButton — кнопка-текст с цветом; enabled=false — серая, клики игнорируются.
func flatButton(gtx layout.Context, th *material.Theme, btn *widget.Clickable,
	txt string, enabled bool, c color.NRGBA, fn func()) layout.Dimensions {
	for btn.Clicked(gtx) {
		if enabled {
			fn()
		}
	}
	col := c
	if !enabled {
		col = color.NRGBA{R: c.R, G: c.G, B: c.B, A: 0x55}
	}
	return material.Clickable(gtx, btn, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th, txt)
			lbl.Color = col
			lbl.TextSize = unit.Sp(14)
			return lbl.Layout(gtx)
		})
	})
}

func dot(gtx layout.Context, c color.NRGBA, d int) layout.Dimensions {
	paint.FillShape(gtx.Ops, c,
		clip.Ellipse(image.Rect(0, 0, d, d)).Op(gtx.Ops))
	return layout.Dimensions{Size: image.Pt(d, d)}
}

func centeredText(gtx layout.Context, th *material.Theme, txt string) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body1(th, txt)
		lbl.Color = colFgDim
		return lbl.Layout(gtx)
	})
}

// fillBehind рисует фон строго по размеру контента (а не по всем остаткам
// места, как Expanded в Stack).
func fillBehind(gtx layout.Context, bg color.NRGBA, content layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := content(gtx)
	call := macro.Stop()
	paint.FillShape(gtx.Ops, bg, clip.Rect{Max: dims.Size}.Op())
	call.Add(gtx.Ops)
	return dims
}

// fillBehindBar — то же плюс цветная полоска слева (карточка идентичности).
func fillBehindBar(gtx layout.Context, bg, bar color.NRGBA, barW int, content layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := content(gtx)
	call := macro.Stop()
	paint.FillShape(gtx.Ops, bg, clip.Rect{Max: dims.Size}.Op())
	paint.FillShape(gtx.Ops, bar, clip.Rect{Max: image.Pt(barW, dims.Size.Y)}.Op())
	call.Add(gtx.Ops)
	return dims
}

// --------------------------------------------- single-instance и запуск окна

// lockHandle держит flock первичного экземпляра весь срок жизни процесса.
var lockHandle *os.File

// bindInstanceLock гарантирует один запущенный экземпляр: первенство
// закрепляется flock-ом (без гонок), IPC-сокет служит для команды «show».
// Если экземпляр уже работает — просит его открыть окно и возвращает false.
func bindInstanceLock() (net.Listener, bool) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}

	lf, err := os.OpenFile(filepath.Join(dir, "ziti-gui.lock"),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			// первичный уже работает (даже если завис) — вторым не становимся
			lf.Close()
			for i := 0; i < 5; i++ {
				c, derr := net.Dial("unix", filepath.Join(dir, "ziti-gui.sock"))
				if derr == nil {
					_, _ = c.Write([]byte("show\n"))
					time.Sleep(300 * time.Millisecond)
					_ = c.Close()
					return nil, false
				}
				time.Sleep(200 * time.Millisecond)
			}
			notify("ZITI-GUI уже запущен", "Но его окно открыть не удалось")
			return nil, false
		}
		lockHandle = lf // не закрывать: flock держится, пока жив процесс
	}

	path := filepath.Join(dir, "ziti-gui.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.Remove(path) // висячий сокет (флока нет — значит, хозяин мёртв)
		ln, err = net.Listen("unix", path)
		if err != nil {
			return nil, true // живём без IPC, единственными
		}
	}
	return ln, true
}

// serveInstanceLock принимает команды от вторых экземпляров: show/quit.
func serveInstanceLock(ln net.Listener, a *app) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 32)
				n, _ := c.Read(buf)
				switch strings.TrimSpace(string(buf[:n])) {
				case "show":
					a.openWindow()
				case "quit":
					a.quitApp()
				}
			}(c)
		}
	}()
}
