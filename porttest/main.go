// porttest — диагностика xdg-desktop-portal FileChooser: логирует все сигналы,
// приходящие на соединение, и ответ портала на OpenFile.
package main

import (
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

func main() {
	conn, err := dbus.SessionBus()
	if err != nil {
		panic(err)
	}

	sigCh := make(chan *dbus.Signal, 64)
	conn.Signal(sigCh)
	go func() {
		for sig := range sigCh {
			fmt.Printf("SIGNAL path=%s member=%s body=%v\n",
				sig.Path, sig.Name, sig.Body)
		}
	}()

	if err := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		"type='signal',interface='org.freedesktop.portal.Request'").Err; err != nil {
		fmt.Println("AddMatch err:", err)
	}

	token := fmt.Sprintf("porttest%d", time.Now().UnixNano())
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"accept_label": dbus.MakeVariant("Выбрать"),
	}
	var handle dbus.ObjectPath
	obj := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	fmt.Println("открываю диалог...")
	err = obj.Call("org.freedesktop.portal.FileChooser.OpenFile", 0,
		"", "porttest: выбери любой файл", opts).Store(&handle)
	fmt.Printf("handle=%s err=%v\n", handle, err)
	fmt.Println("жду сигналы до 10 минут (выбери файл или отмена)...")
	time.Sleep(10 * time.Minute)
}
