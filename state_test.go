package main

import (
	"encoding/json"
	"testing"
)

// Фрагмент реального вывода `ziti-edge-tunnel dump` v1.18.7.
const realDumpText = `Ziti Context:
ID:	2
enabled[true] uptime[22948s]
Identity:	t490[vZRzKYijE]
TOTP: enrolled[N] required[N]
API Session:
	auth_state[FullyAuthenticated]
Services:
dpn.home.lan: id[1sZlfkbsVyRlW14q6zP8nL] perm(dial=true,bind=false)
	config[intercept.v1]={ "addresses": [ "dpn.home.lan" ], "portRanges": [ { "high": 80, "low": 80 } ], "protocols": [ "tcp", "udp" ] }
	config[host.v1]={ "address": "11.22.33.44" }
	posture queries[1]:		posture query set[dummy]
tfs-main.ziti: id[20DVZYDxVgF4rN5jaeg8FY] perm(dial=true,bind=false)
	config[intercept.v1]={ "addresses": [ "tfs-main.ziti", "tfs-main" ], "portRanges": [ { "high": 8080, "low": 8080 } ], "protocols": [ "tcp", "udp" ] }
tfs-bind.ziti: id[22fMlAXkmwRA3k1OOZPiEp] perm(dial=false,bind=true)
	config[intercept.v1]={ "addresses": [ "tfs-bind.ziti" ], "portRanges": [ { "high": 65535, "low": 1 } ], "protocols": [ "tcp" ] }`

func TestParseDumpRealFormat(t *testing.T) {
	// текст отчёта попадает в JSON строкой — собираем так же, как туннелер
	payload, err := json.Marshal(map[string]string{
		"/opt/openziti/etc/identities/voev-t490.json": realDumpText,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := `{"Success":true,"Data":` + string(payload) + `}`
	idents, err := parseDump(in)
	if err != nil {
		t.Fatalf("parseDump: %v", err)
	}
	if len(idents) != 1 {
		t.Fatalf("идентичностей: %d, ждём 1", len(idents))
	}
	id := idents[0]
	if id.Name != "t490" || id.ID != "vZRzKYijE" {
		t.Errorf("Name/ID: %q/%q", id.Name, id.ID)
	}
	if !id.Enabled || !id.EnabledKnown {
		t.Errorf("Enabled=%v Known=%v, ждём true/true", id.Enabled, id.EnabledKnown)
	}
	if id.AuthState != "FullyAuthenticated" {
		t.Errorf("AuthState: %q", id.AuthState)
	}
	if id.TOTPRequired || id.TOTPEnrolled {
		t.Errorf("TOTP: enrolled=%v required=%v", id.TOTPEnrolled, id.TOTPRequired)
	}
	if id.UptimeSec != 22948 {
		t.Errorf("UptimeSec: %d", id.UptimeSec)
	}
	if id.File != "/opt/openziti/etc/identities/voev-t490.json" {
		t.Errorf("File: %q", id.File)
	}
	if len(id.Services) != 3 {
		t.Fatalf("сервисов: %d, ждём 3", len(id.Services))
	}
	s0 := id.Services[0]
	if s0.Name != "dpn.home.lan" || s0.Addr != "dpn.home.lan" || s0.Port != "80" || !s0.Dial || s0.Bind {
		t.Errorf("svc0: %+v", s0)
	}
	if id.Services[1].Port != "8080" {
		t.Errorf("svc1 port: %q", id.Services[1].Port)
	}
	// ВСЕ intercept-адреса, а не только первый
	if len(id.Services[1].Addrs) != 2 || id.Services[1].Addrs[0] != "tfs-main.ziti" || id.Services[1].Addrs[1] != "tfs-main" {
		t.Errorf("svc1 Addrs: %v", id.Services[1].Addrs)
	}
	if id.Services[1].Addr != "tfs-main.ziti" {
		t.Errorf("svc1 Addr: %q", id.Services[1].Addr)
	}
	sb := id.Services[2]
	if sb.Port != "1-65535" || !sb.Bind || sb.Dial {
		t.Errorf("svc bind: %+v", sb)
	}
	if got := sb.label(); got != "tfs-bind.ziti · tfs-bind.ziti:1-65535 (bind)" {
		t.Errorf("label: %q", got)
	}
}

func TestParseDumpVariants(t *testing.T) {
	cases := []struct {
		name             string
		in               string
		wantIdents       int
		wantEnabled      bool
		wantEnabledKnown bool
		wantSvc          int
		wantAddr         string
	}{
		{
			name:       "строгий легаси-формат",
			in:         `{"Identities":[{"Name":"foo","Id":"abc","Enabled":true,"Services":[{"Name":"web","AssignedIP":"100.64.0.1"}]}]}`,
			wantIdents: 1, wantEnabled: true, wantEnabledKnown: true, wantSvc: 1, wantAddr: "100.64.0.1",
		},
		{
			name:       "мусор перед JSON",
			in:         "WARN: something bad\nINFO: log\n{\"Identities\":[{\"Name\":\"foo\",\"Enabled\":false,\"Services\":[]}]}",
			wantIdents: 1, wantEnabled: false, wantEnabledKnown: true,
		},
		{
			name:       "массив идентичностей",
			in:         `[{"Name":"bar","Services":[{"Name":"db","AssignedIP":"100.64.0.2"}]}]`,
			wantIdents: 1, wantEnabled: true, wantSvc: 1, wantAddr: "100.64.0.2",
		},
		{
			name:       "lowercase ключи (generic-скан)",
			in:         `{"identities":[{"name":"baz","enabled":false,"services":[{"name":"api","ip":"100.64.0.3"}]}]}`,
			wantIdents: 1, wantEnabled: false, wantEnabledKnown: true, wantSvc: 1, wantAddr: "100.64.0.3",
		},
		{
			name:       "несколько идентичностей",
			in:         `{"Identities":[{"Name":"a"},{"Name":"b","Enabled":false}]}`,
			wantIdents: 2, wantEnabled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idents, err := parseDump(tc.in)
			if err != nil {
				t.Fatalf("parseDump: %v", err)
			}
			if len(idents) != tc.wantIdents {
				t.Fatalf("идентичностей: %d, ждём %d (%+v)", len(idents), tc.wantIdents, idents)
			}
			id := idents[0]
			if id.Enabled != tc.wantEnabled {
				t.Errorf("Enabled: %v, ждём %v", id.Enabled, tc.wantEnabled)
			}
			if id.EnabledKnown != tc.wantEnabledKnown {
				t.Errorf("EnabledKnown: %v, ждём %v", id.EnabledKnown, tc.wantEnabledKnown)
			}
			if len(id.Services) != tc.wantSvc {
				t.Errorf("сервисов: %d, ждём %d", len(id.Services), tc.wantSvc)
			}
			if tc.wantAddr != "" && (len(id.Services) == 0 || id.Services[0].Addr != tc.wantAddr) {
				t.Errorf("Addr: %v, ждём %s", id.Services, tc.wantAddr)
			}
		})
	}
}

func TestParseDumpValidEmpty(t *testing.T) {
	for _, in := range []string{
		`{"Success":true,"Data":{}}`,
		`{"Success":true}`,
		`{"Identities":[]}`,
	} {
		if idents, err := parseDump(in); err != nil || len(idents) != 0 {
			t.Errorf("валидный пустой dump %q: err=%v idents=%d", in, err, len(idents))
		}
	}
}

func TestParseDumpErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"no json here",
		`{"Success":false,"Error":"boom"}`,
	} {
		if _, err := parseDump(in); err == nil {
			t.Errorf("ожидали ошибку для %q", in)
		}
	}
}

func TestExtractJSONWithTrailingGarbage(t *testing.T) {
	out := `{"Identities":[{"Name":"x"}]}` + "\nWARN: trailing log"
	raw, ok := extractJSON(out)
	if !ok {
		t.Fatal("JSON не найден")
	}
	idents, err := parseDump(raw)
	if err != nil || len(idents) != 1 || idents[0].Name != "x" {
		t.Fatalf("parse: %v %+v", err, idents)
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("/tmp/my token (1).jwt"); got != "my_token__1_" {
		t.Errorf("sanitizeName: %q", got)
	}
	if got := sanitizeName(""); len(got) == 0 {
		t.Error("пустое имя")
	}
}
