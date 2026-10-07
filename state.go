package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------ модель статуса

type ServiceState string

const (
	SvcActive     ServiceState = "active"
	SvcActivating ServiceState = "activating"
	SvcInactive   ServiceState = "inactive"
	SvcFailed     ServiceState = "failed"
	SvcUnknown    ServiceState = "unknown"
)

// ServiceInfo — сервис Ziti внутри идентичности.
type ServiceInfo struct {
	Name  string
	Addr  string   // первый intercept-адрес (для компактных подписей)
	Addrs []string // ВСЕ intercept-адреса (хосты или CIDR)
	Port  string   // "80", "22,8006", "1-65535"
	Dial  bool
	Bind  bool
}

func (s ServiceInfo) label() string {
	parts := s.Name
	if s.Addr != "" {
		p := s.Port
		if p != "" {
			p = ":" + p
		}
		parts += " · " + s.Addr + p
	}
	if s.Bind && !s.Dial {
		parts += " (bind)"
	}
	return parts
}

type Ident struct {
	Name         string // отображаемое имя (из dump или имя файла)
	ID           string // id из dump
	File         string // путь к файлу идентичности
	Enabled      bool   // включена в работающем туннелере
	EnabledKnown bool   // удалось ли узнать из dump
	AuthState    string // FullyAuthenticated, ...
	TOTPEnrolled bool
	TOTPRequired bool
	UptimeSec    int
	IPs          []string // легаси-формат dump
	Services     []ServiceInfo
}

// NeedsAttention — идентичность требует внимания (MFA, недоавторизована).
func (id Ident) NeedsAttention() bool {
	if id.TOTPRequired && !id.TOTPEnrolled {
		return true
	}
	return id.AuthState != "" && id.AuthState != "FullyAuthenticated"
}

type Status struct {
	Svc     ServiceState
	PID     string
	SvcUser string
	IpcOK   bool
	IpcErr  string
	Idents  []Ident
	Fetched time.Time
}

func (s Status) key() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%t", s.Svc, s.PID, s.IpcOK)
	for _, id := range s.Idents {
		names := make([]string, len(id.Services))
		for i, svc := range id.Services {
			names[i] = svc.Name
		}
		fmt.Fprintf(&b, "|%s:%t:%t:%d:%s:%v", id.Name, id.Enabled, id.EnabledKnown,
			len(id.Services), id.AuthState, names)
	}
	return b.String()
}

// ------------------------------------------------------------------- парсинг

// Реальный формат dump (ziti-edge-tunnel v1.18.x):
//
//	{"Success":true,"Data":{"/путь/файл.json":"<текстовый отчёт>", ...}}
//
// Текстовый отчёт содержит строки вида:
//
//	Identity:	t490[vZRzKYijE]
//	enabled[true] uptime[22948s]
//	auth_state[FullyAuthenticated]
//	TOTP: enrolled[N] required[N]
//	Services:
//	dpn.home.lan: id[1sZlfk…] perm(dial=true,bind=false)
//		config[intercept.v1]={ "addresses": [...], "portRanges": [...] }
type dumpOut struct {
	Success    *bool                   `json:"Success"`
	Error      string                  `json:"Error"`
	Data       map[string]string       `json:"Data"`
	Identities dumpOut_IdentitiesSlice `json:"Identities"` // легаси
}

type dumpService struct {
	Name string
	IPs  []string
}

// UnmarshalJSON терпит разные схемы поля IP в разных версиях ziti-edge-tunnel.
func (s *dumpService) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k, v := range m {
		switch strings.ToLower(k) {
		case "name":
			if str, ok := v.(string); ok {
				s.Name = str
			}
		case "assignedip", "assigned_ip", "ip", "address":
			if str, ok := v.(string); ok && str != "" {
				s.IPs = append(s.IPs, str)
			}
		case "addresses":
			if arr, ok := v.([]any); ok {
				for _, e := range arr {
					if str, ok := e.(string); ok && str != "" {
						s.IPs = append(s.IPs, str)
					}
				}
			}
		}
	}
	return nil
}

type dumpOut_Identity = struct {
	Name     string          `json:"Name"`
	Id       string          `json:"Id"`
	Enabled  *bool           `json:"Enabled"`
	Config   json.RawMessage `json:"Config"`
	Services []dumpService   `json:"Services"`
}

type dumpOut_IdentitiesSlice = []dumpOut_Identity

func parseDump(out string) ([]Ident, error) {
	raw, ok := extractJSON(out)
	if !ok {
		return nil, fmt.Errorf("в выводе dump нет JSON")
	}
	var d dumpOut
	_ = json.Unmarshal([]byte(raw), &d) // нет ошибке рушить fallback-цепочку
	if d.Error != "" {
		return nil, fmt.Errorf("%s", d.Error)
	}
	// основной формат: текстовые отчёты по файлам
	if len(d.Data) > 0 {
		return parseDumpData(d.Data), nil
	}
	// легаси: объект/массив идентичностей
	if len(d.Identities) > 0 {
		return fromDumpIdentities(d.Identities), nil
	}
	if (d.Success != nil && *d.Success) || d.Data != nil {
		return []Ident{}, nil // валидный dump без идентичностей
	}
	var arr dumpOut_IdentitiesSlice
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return fromDumpIdentities(arr), nil
	}
	// запасной вариант: регистронезависимый поиск идентичностей в любой структуре
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	var idents []Ident
	genericScan(v, &idents)
	if len(idents) > 0 {
		return idents, nil
	}
	return []Ident{}, nil
}

// extractJSON вырезает первый валидный JSON-документ из вывода с возможным
// мусором до и после (логи в stderr склеиваются с stdout через CombinedOutput).
func extractJSON(out string) (string, bool) {
	for i := 0; i < len(out); i++ {
		if out[i] != '{' && out[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(out[i:]))
		var v any
		if err := dec.Decode(&v); err == nil {
			return out[i : i+int(dec.InputOffset())], true
		}
	}
	return "", false
}

// --------------------------------------------------- парсинг текстовых отчётов

var (
	reDumpIdentity  = regexp.MustCompile(`(?m)^Identity:\s+(\S+)\[([^\]]+)\]`)
	reDumpEnabled   = regexp.MustCompile(`enabled\[(true|false)\]`)
	reDumpUptime    = regexp.MustCompile(`uptime\[(\d+)s\]`)
	reDumpAuth      = regexp.MustCompile(`auth_state\[([^\]]+)\]`)
	reDumpTOTP      = regexp.MustCompile(`TOTP:\s*enrolled\[(Y|N)\]\s*required\[(Y|N)\]`)
	reDumpService   = regexp.MustCompile(`(?m)^([^\s:][^:\n]*?): id\[([^\]]*)\] perm\(dial=(true|false),bind=(true|false)\)`)
	reDumpIntercept = regexp.MustCompile(`config\[intercept\.v1\]=(\{[^\n]*)`)
)

type portRange struct {
	Low  int `json:"low"`
	High int `json:"high"`
}

type interceptCfg struct {
	Addresses  []string    `json:"addresses"`
	PortRanges []portRange `json:"portRanges"`
}

func parseDumpData(data map[string]string) []Ident {
	files := sortedNames(data)
	idents := make([]Ident, 0, len(files))
	for _, file := range files {
		text := data[file]
		id := Ident{File: file, Enabled: true}
		if m := reDumpIdentity.FindStringSubmatch(text); m != nil {
			id.Name, id.ID = m[1], m[2]
		}
		if id.Name == "" {
			id.Name = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		}
		if m := reDumpEnabled.FindStringSubmatch(text); m != nil {
			id.Enabled = m[1] == "true"
			id.EnabledKnown = true
		}
		if m := reDumpUptime.FindStringSubmatch(text); m != nil {
			id.UptimeSec, _ = strconv.Atoi(m[1])
		}
		if m := reDumpAuth.FindStringSubmatch(text); m != nil {
			id.AuthState = m[1]
		}
		if m := reDumpTOTP.FindStringSubmatch(text); m != nil {
			id.TOTPEnrolled = m[1] == "Y"
			id.TOTPRequired = m[2] == "Y"
		}
		id.Services = parseServices(text)
		idents = append(idents, id)
	}
	return idents
}

func parseServices(text string) []ServiceInfo {
	matches := reDumpService.FindAllStringSubmatchIndex(text, -1)
	out := make([]ServiceInfo, 0, len(matches))
	for i, m := range matches {
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		svc := ServiceInfo{
			Name: text[m[2]:m[3]],
			Dial: text[m[6]:m[7]] == "true",
			Bind: text[m[8]:m[9]] == "true",
		}
		// intercept-конфигов у сервиса может быть несколько — собираем все
		var ranges []portRange
		for _, im := range reDumpIntercept.FindAllStringSubmatch(text[m[1]:end], -1) {
			var cfg interceptCfg
			if json.Unmarshal([]byte(im[1]), &cfg) == nil {
				svc.Addrs = append(svc.Addrs, cfg.Addresses...)
				ranges = append(ranges, cfg.PortRanges...)
			}
		}
		svc.Port = joinPorts(ranges)
		if len(svc.Addrs) > 0 {
			svc.Addr = svc.Addrs[0]
		}
		out = append(out, svc)
	}
	return out
}

func joinPorts(ranges []portRange) string {
	if len(ranges) == 0 {
		return ""
	}
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		if r.Low == r.High {
			parts[i] = strconv.Itoa(r.Low)
		} else {
			parts[i] = strconv.Itoa(r.Low) + "-" + strconv.Itoa(r.High)
		}
	}
	return strings.Join(parts, ",")
}

func fromDumpIdentities(list dumpOut_IdentitiesSlice) []Ident {
	idents := make([]Ident, 0, len(list))
	for _, di := range list {
		if di.Name == "" {
			continue
		}
		id := Ident{Name: di.Name, ID: di.Id, Enabled: true, EnabledKnown: di.Enabled != nil}
		if di.Enabled != nil {
			id.Enabled = *di.Enabled
		}
		for _, s := range di.Services {
			svc := ServiceInfo{Name: s.Name}
			if len(s.IPs) > 0 {
				svc.Addr = s.IPs[0]
				id.IPs = append(id.IPs, s.IPs...)
			}
			id.Services = append(id.Services, svc)
		}
		idents = append(idents, id)
	}
	return idents
}

func genericScan(v any, out *[]Ident) {
	switch t := v.(type) {
	case []any:
		if ids := tryAsIdents(t); ids != nil {
			*out = append(*out, ids...)
			return
		}
		for _, e := range t {
			genericScan(e, out)
		}
	case map[string]any:
		for k, e := range t {
			if strings.EqualFold(k, "identities") {
				genericScan(e, out)
				return
			}
		}
		for _, e := range t {
			genericScan(e, out)
		}
	}
}

func tryAsIdents(arr []any) []Ident {
	var ids []Ident
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil
		}
		name := strField(m, "name")
		if name == "" {
			return nil
		}
		id := Ident{Name: name, Enabled: true}
		if en, ok := boolField(m, "enabled"); ok {
			id.Enabled = en
			id.EnabledKnown = true
		}
		if idv, ok := anyField(m, "services"); ok {
			if svcs, ok := idv.([]any); ok {
				for _, sv := range svcs {
					sm, ok := sv.(map[string]any)
					if !ok {
						continue
					}
					svc := ServiceInfo{Name: strField(sm, "name")}
					for _, k := range []string{"assignedip", "assigned_ip", "ip", "address"} {
						if ip := strField(sm, k); ip != "" {
							svc.Addr = ip
							break
						}
					}
					id.Services = append(id.Services, svc)
				}
			}
		}
		ids = append(ids, id)
	}
	return ids
}

func anyField(m map[string]any, key string) (any, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

func strField(m map[string]any, key string) string {
	if v, ok := anyField(m, key); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) (bool, bool) {
	if v, ok := anyField(m, key); ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

// ------------------------------------------------------------------ приложение

type app struct {
	cfg *Config

	mu       sync.Mutex
	cur      Status
	ver      string          // подпись последнего отрисованного состояния
	jobs     sync.Map        // однократные полёты действий
	delArmed map[string]bool // подтверждённое удаление (двухшаговое)

	enrollBusy     atomic.Bool          // диалог/процесс регистрации уже идёт
	missingSince   map[string]time.Time // pid/файл -> с каких пор контекст отсутствует
	recoverTried   map[string]string    // файл -> MainPID сервиса, для которого уже пробовали
	applyOffAt     map[string]time.Time // имя -> когда последний раз применяли "выкл"
	pendingToggles map[string]time.Time // имя -> когда последний раз кликнули тоггл

	updateCh chan struct{}
	quit     func()
}

func newApp(cfg *Config) *app {
	return &app{
		cfg:      cfg,
		delArmed: map[string]bool{},
		updateCh: make(chan struct{}, 1),
	}
}

func (a *app) requestUpdate() {
	select {
	case a.updateCh <- struct{}{}:
	default:
	}
}

// poll собирает текущее состояние: systemd + IPC dump.
func (a *app) poll() Status {
	ctx := context.Background()
	st := Status{Fetched: time.Now()}

	// один вызов вместо двух; парсим "Ключ=Значение" — systemctl выводит
	// свойства в своём порядке, позиционный разбор тут ломается
	out, _ := a.systemctl(ctx, "show", "-p", "ActiveState", "-p", "MainPID", a.cfg.Service)
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			st.Svc = SvcUnknown
			switch strings.TrimSpace(v) {
			case "active":
				st.Svc = SvcActive
			case "activating", "reloading":
				st.Svc = SvcActivating
			case "failed":
				st.Svc = SvcFailed
			case "inactive", "maintenance", "degrading":
				st.Svc = SvcInactive
			}
		case "MainPID":
			st.PID = strings.TrimSpace(v)
		}
	}
	if st.Svc == "" {
		st.Svc = SvcUnknown
	}
	st.SvcUser = a.svcUser()

	if st.Svc == SvcActive || st.Svc == SvcActivating {
		raw, err := a.zitiClient(ctx, 10*time.Second, "dump")
		if err != nil {
			st.IpcErr = shortErr(err, raw)
		} else if idents, perr := parseDump(raw); perr == nil {
			st.IpcOK = true
			st.Idents = idents
		} else {
			st.IpcErr = "dump: " + perr.Error()
		}
	}

	a.mergeFiles(&st)

	// сохранённые "выключенные" должны гаситься и при поздней загрузке контекста
	for _, id := range st.Idents {
		if !id.EnabledKnown || !id.Enabled {
			continue
		}
		off := false
		a.mu.Lock()
		if a.cfg.Disabled[id.Name] {
			off = true
		}
		if b := fileBase(id.File); b != "" && a.cfg.Disabled[b] {
			off = true
		}
		a.mu.Unlock()
		if off {
			a.mu.Lock()
			last, seen := a.applyOffAt[id.Name]
			if !seen {
				if a.applyOffAt == nil {
					a.applyOffAt = map[string]time.Time{}
				}
				a.applyOffAt[id.Name] = time.Time{}
				seen = true
			}
			now := time.Now()
			throttled := seen && !last.IsZero() && now.Sub(last) < 60*time.Second
			if !throttled {
				a.applyOffAt[id.Name] = now
			}
			a.mu.Unlock()
			if !throttled {
				go a.applyPersistedDisables(st)
			}
		}
	}

	a.mu.Lock()
	prev := a.cur
	a.cur = st
	a.mu.Unlock()

	// туннелер только что поднялся — применяем сохранённые вкл/выкл
	if st.IpcOK && (!prev.IpcOK || prev.Svc != SvcActive) {
		go a.applyPersistedDisables(st)
	}
	// зависшие при старте контексты (файл есть, в dump нет) — переинициируем
	a.maybeRecoverMissing(st)
	return st
}

func shortErr(err error, raw string) string {
	msg := strings.TrimSpace(raw)
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return msg
}

// maybeRecoverMissing перезапускает ("выкл/вкл") контексты, не загрузившиеся
// при старте сервиса: файла нет в dump, но он есть в каталоге. Одна попытка
// на файл на каждый запуск сервиса (ключ — MainPID), не раньше 20 секунд
// после первого отсутствия, выключенным по конфигу не трогаем.
func (a *app) maybeRecoverMissing(st Status) {
	if !st.IpcOK || st.Svc != SvcActive || st.PID == "" || st.PID == "0" {
		return
	}
	now := time.Now()
	a.mu.Lock()
	if a.missingSince == nil {
		a.missingSince = map[string]time.Time{}
	}
	if a.recoverTried == nil {
		a.recoverTried = map[string]string{}
	}
	var todo []Ident
	for _, id := range st.Idents {
		if id.EnabledKnown || id.File == "" {
			continue
		}
		if a.cfg.Disabled[id.Name] {
			continue
		}
		if b := fileBase(id.File); b != "" && a.cfg.Disabled[b] {
			continue
		}
		key := st.PID + "/" + id.File
		if a.recoverTried[id.File] == st.PID {
			continue
		}
		t0, ok := a.missingSince[key]
		if !ok {
			a.missingSince[key] = now
			continue
		}
		if now.Sub(t0) < 20*time.Second {
			continue
		}
		a.recoverTried[id.File] = st.PID
		delete(a.missingSince, key)
		todo = append(todo, id)
	}
	a.mu.Unlock()

	for _, id := range todo {
		id := id
		go func() {
			log.Printf("recover: контекст %s не загрузился — переинициализация", id.Name)
			if out, err := a.zitiClient(context.Background(), 10*time.Second,
				"on_off_identity", "-i", id.File, "-o", "f"); zitiIPCError(out, err) == "" {
				time.Sleep(time.Second)
			}
			out, err := a.zitiClient(context.Background(), 15*time.Second,
				"on_off_identity", "-i", id.File, "-o", "t")
			if msg := zitiIPCError(out, err); msg != "" && !strings.Contains(msg, "not found") {
				log.Printf("recover %s: FAIL %s", id.Name, msg)
				return
			}
			log.Printf("recover %s: ok", id.Name)
			notify("Ziti", "Идентичность «"+id.Name+"» переподключена")
			a.requestUpdate()
		}()
	}
}

// mergeFiles добавляет идентичности из файлов, которых нет в dump.
func (a *app) mergeFiles(st *Status) {
	files := a.listIdentityFiles()
	known := map[string]*Ident{}
	for i := range st.Idents {
		known[strings.ToLower(st.Idents[i].Name)] = &st.Idents[i]
		if st.Idents[i].File == "" {
			if p, ok := files[st.Idents[i].Name]; ok {
				st.Idents[i].File = p
			}
		}
	}
	for base := range files {
		// не дублируем идентичности из dump: совпадение по имени ИЛИ по файлу
		dup := false
		for i := range st.Idents {
			if st.Idents[i].File == files[base] || strings.EqualFold(st.Idents[i].Name, base) {
				dup = true
				break
			}
		}
		if !dup {
			a.mu.Lock()
			off := a.cfg.Disabled[base]
			if b := fileBase(files[base]); b != "" && a.cfg.Disabled[b] {
				off = true
			}
			a.mu.Unlock()
			st.Idents = append(st.Idents, Ident{
				Name:    base,
				File:    files[base],
				Enabled: !off,
			})
		}
	}
	sort.Slice(st.Idents, func(i, j int) bool { return st.Idents[i].Name < st.Idents[j].Name })
}

// identArg возвращает идентификатор для клиентских команд туннелера.
// IPC-команды (on_off_identity, delete, refresh) принимают только полный
// путь к файлу идентичности — по короткому имени дают "ziti context not found".
func (a *app) identArg(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range a.cur.Idents {
		if id.Name == name && id.File != "" {
			return id.File
		}
	}
	return name
}

// togglePending — по имени не так давно кликали тоггл: не синхронизировать
// переключатель с состоянием туннелера, чтобы он не «мигал» обратно.
func (a *app) togglePending(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.pendingToggles[name]
	return ok && time.Since(t) < 5*time.Second
}

func (a *app) clearTogglePending(name string) {
	a.mu.Lock()
	delete(a.pendingToggles, name)
	a.mu.Unlock()
}

// zitiIPCError проверяет ответ клиентской команды: CLI завершается с кодом 0
// даже при ошибке, статус надо читать из JSON ("Success":false).
func zitiIPCError(out string, err error) string {
	// CLI отдаёт pretty-JSON и часто с кодом 0 даже при ошибке — парсим всегда
	raw, ok := extractJSON(out)
	if ok {
		var r struct {
			Success *bool
			Error   string
		}
		if json.Unmarshal([]byte(raw), &r) == nil {
			if r.Error != "" {
				return r.Error
			}
			if r.Success != nil && !*r.Success {
				return "операция не выполнена"
			}
			return "" // Success:true
		}
	}
	if err != nil {
		return firstLine(out, err)
	}
	return ""
}

// --------------------------------------------------------------- действия

func (a *app) once(key string, fn func()) {
	if _, loaded := a.jobs.LoadOrStore(key, true); loaded {
		return
	}
	go func() {
		defer a.jobs.Delete(key)
		fn()
	}()
}

func (a *app) svcStart() {
	a.once("svc", func() {
		if out, err := a.systemctlPriv(context.Background(), "start", a.cfg.Service); err != nil {
			notify("Ziti: не удалось запустить", friendlyErr(out, err))
		}
		time.Sleep(300 * time.Millisecond)
		a.requestUpdate()
	})
}

func (a *app) svcStop() {
	a.once("svc", func() {
		if out, err := a.systemctlPriv(context.Background(), "stop", a.cfg.Service); err != nil {
			notify("Ziti: не удалось остановить", friendlyErr(out, err))
		}
		time.Sleep(300 * time.Millisecond)
		a.requestUpdate()
	})
}

func (a *app) svcRestart() {
	a.once("svc", func() {
		if out, err := a.systemctlPriv(context.Background(), "restart", a.cfg.Service); err != nil {
			notify("Ziti: не удалось перезапустить", friendlyErr(out, err))
		}
		time.Sleep(500 * time.Millisecond)
		a.requestUpdate()
	})
}

// toggleIdent — вкл/выкл идентичности: живьём через IPC + сохранение в конфиг.
func (a *app) toggleIdent(name string, enable bool) {
	a.mu.Lock()
	cur := a.cur
	known := false
	for _, id := range cur.Idents {
		if id.Name == name {
			known = id.EnabledKnown
			break
		}
	}
	a.mu.Unlock()

	// важно: identArg лочит a.mu, поэтому вызываем вне критических секций
	ident := a.identArg(name)

	// контекст не загружен (упал при старте сервиса): включение — это цикл
	// выкл/вкл (переинициализация), выключение — только сохранённое намерение
	if !known {
		log.Printf("toggle %s: контекст не загружен, enable=%t", name, enable)
		if enable {
			if out, err := a.zitiClient(context.Background(), 10*time.Second,
				"on_off_identity", "-i", ident, "-o", "f"); zitiIPCError(out, err) == "" {
				time.Sleep(time.Second)
			}
			out, err := a.zitiClient(context.Background(), 15*time.Second,
				"on_off_identity", "-i", ident, "-o", "t")
			if msg := zitiIPCError(out, err); msg != "" && !strings.Contains(msg, "not found") {
				log.Printf("toggle %s: FAIL %s", name, msg)
				notify("Ziti: не удалось загрузить", msg+" — попробуйте позже или перезапустите сервис")
				a.clearTogglePending(name)
				a.requestUpdate()
				return
			}
		} else if cur.Svc == SvcActive {
			notify("Ziti", "«"+name+"» не загружена — выключение применится при загрузке")
		}
		a.mu.Lock()
		if enable {
			delete(a.cfg.Disabled, name)
			if b := fileBase(ident); b != "" {
				delete(a.cfg.Disabled, b)
			}
		} else {
			a.cfg.Disabled[name] = true
			if b := fileBase(ident); b != "" {
				a.cfg.Disabled[b] = true
			}
		}
		a.cfg.save()
		a.mu.Unlock()
		a.requestUpdate()
		return
	}

	if cur.IpcOK {
		arg := "f"
		if enable {
			arg = "t"
		}
		out, err := a.zitiClient(context.Background(), 10*time.Second,
			"on_off_identity", "-i", ident, "-o", arg)
		if msg := zitiIPCError(out, err); msg != "" {
			log.Printf("toggle %s -> %s: FAIL %s", name, arg, msg)
			notify("Ziti: переключение не применено", msg)
			a.clearTogglePending(name)
			a.requestUpdate()
			return
		}
		log.Printf("toggle %s -> %s: ok", name, arg)
		a.clearTogglePending(name)
	} else if cur.Svc == SvcActive {
		notify("Ziti", "Нет доступа к IPC: состояние применится после перезапуска сервиса")
	}

	// намерение сохраняем только после успешного IPC (когда сервис остановлен —
	// это единственное место, где хранить состояние)
	a.mu.Lock()
	if enable {
		delete(a.cfg.Disabled, name)
		if b := fileBase(ident); b != "" {
			delete(a.cfg.Disabled, b)
		}
	} else {
		a.cfg.Disabled[name] = true
		if b := fileBase(ident); b != "" {
			a.cfg.Disabled[b] = true
		}
	}
	a.cfg.save()
	a.mu.Unlock()
	a.requestUpdate()
}

// fileBase — имя файла идентичности без пути и расширения ("" для пустого).
func fileBase(path string) string {
	if path == "" {
		return ""
	}
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// applyPersistedDisables — после старта туннелера выключаем то, что выключено в конфиге.
func (a *app) applyPersistedDisables(st Status) {
	if !st.IpcOK {
		return
	}
	for _, id := range st.Idents {
		a.mu.Lock()
		off := a.cfg.Disabled[id.Name]
		if b := fileBase(id.File); b != "" && a.cfg.Disabled[b] {
			off = true
		}
		a.mu.Unlock()
		if !off || !id.Enabled {
			continue
		}
		ident := id.File
		if ident == "" {
			ident = id.Name
		}
		out, err := a.zitiClient(context.Background(), 10*time.Second,
			"on_off_identity", "-i", ident, "-o", "f")
		if zitiIPCError(out, err) == "" {
			log.Printf("apply persisted disable: %s", id.Name)
			notify("Ziti", "Идентичность «"+id.Name+"» выключена (сохранённое состояние)")
			a.clearTogglePending(id.Name)
		} else {
			log.Printf("apply persisted disable %s: FAIL %s", id.Name, zitiIPCError(out, err))
		}
	}
	a.requestUpdate()
}

// refreshIdent — перечитать сервисы с контроллера.
func (a *app) refreshIdent(name string) {
	a.once("refresh:"+name, func() {
		ident := a.identArg(name)
		a.mu.Lock()
		known := false
		for _, id := range a.cur.Idents {
			if id.Name == name {
				known = id.EnabledKnown
				break
			}
		}
		a.mu.Unlock()

		// контекст не загружен (упал при старте/аутентификации): refresh для
		// него бесполезен, переинициируем циклом выкл/вкл
		if !known {
			if out, err := a.zitiClient(context.Background(), 10*time.Second,
				"on_off_identity", "-i", ident, "-o", "f"); zitiIPCError(out, err) == "" {
				time.Sleep(time.Second)
			}
		}
		out, err := a.zitiClient(context.Background(), 30*time.Second, "refresh", "-i", ident)
		if msg := zitiIPCError(out, err); msg != "" {
			log.Printf("refresh %s: FAIL %s", name, msg)
			notify("Ziti: refresh не удался", msg)
		} else {
			log.Printf("refresh %s: ok", name)
			notify("Ziti", "Идентичность «"+name+"» обновлена")
		}
		a.requestUpdate()
	})
}

// deleteIdent — двухшаговое: первый клик ставит «взвод», второй — удаляет.
func (a *app) deleteIdent(id Ident) {
	a.mu.Lock()
	if !a.delArmed[id.Name] {
		a.delArmed[id.Name] = true
		a.mu.Unlock()
		// снимаем "взвод" через минуту, если передумали
		time.AfterFunc(time.Minute, func() {
			a.mu.Lock()
			delete(a.delArmed, id.Name)
			a.mu.Unlock()
			a.requestUpdate()
		})
		a.requestUpdate()
		return
	}
	delete(a.delArmed, id.Name)
	cur := a.cur
	a.mu.Unlock()
	a.once("del:"+id.Name, func() {
		var msgs []string
		if cur.IpcOK {
			out, err := a.zitiClient(context.Background(), 15*time.Second, "delete", "-i", a.identArg(id.Name))
			if msg := zitiIPCError(out, err); msg != "" && !strings.Contains(msg, "not found") {
				msgs = append(msgs, msg)
			}
		}
		if id.File != "" {
			script := fmt.Sprintf("rm -f %q && echo DEL_OK", id.File)
			out, err := runCmd(context.Background(), "pkexec", "/bin/sh", "-c", script, "sh")
			if err != nil || !strings.Contains(out, "DEL_OK") {
				msgs = append(msgs, "файл не удалён: "+friendlyErr(out, err))
			}
		}
		if len(msgs) == 0 {
			notify("Ziti", "Идентичность «"+id.Name+"» удалена")
		} else {
			notify("Ziti: удаление с ошибками", strings.Join(msgs, "; "))
		}
		a.requestUpdate()
	})
}

func firstLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" && err != nil {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// friendlyErr — то же, что firstLine, но с понятной подсказкой, если pkexec
// не смог получить root из-за no_new_privs (приложение запущено из песочницы).
func friendlyErr(out string, err error) string {
	msg := firstLine(out, err)
	joined := out
	if err != nil {
		joined += " " + err.Error()
	}
	if strings.Contains(joined, "must be setuid root") {
		return "pkexec не может получить root: ziti-gui запущен в песочнице " +
			"(no_new_privs). Перезапустите его из обычной сессии"
	}
	return msg
}
