package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

// newTestApp crée une app sans zone de notification, config dans un dossier
// temporaire, passerelle démarrée sur un port libre.
func newTestApp(t *testing.T, zimbraURL string) *app {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux
	t.Setenv("APPDATA", dir)         // Windows
	t.Setenv("HOME", dir)            // macOS
	smtpPort := freePort(t)
	ldapPort := freePort(t)
	for ldapPort == smtpPort {
		ldapPort = freePort(t)
	}
	a := &app{
		cfg: Config{
			ZimbraURL:   zimbraURL,
			ListenPort:  smtpPort,
			LDAPEnabled: true,
			LDAPPort:    ldapPort,
		},
		gw:   NewGateway(zimbraURL),
		ldap: NewLDAPServer(zimbraURL),
	}
	if err := a.startGateway(a.cfg.ListenPort); err != nil {
		t.Fatal(err)
	}
	if err := a.startLDAP(a.cfg.LDAPPort); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stopAll)
	return a
}

func callUI(t *testing.T, a *app, method string, arg any) any {
	t.Helper()
	b, _ := json.Marshal(arg)
	return a.handleUI(method, b)
}

func listening(port int) bool {
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestSaveRejectsInvalidFields(t *testing.T) {
	a := newTestApp(t, "https://old.example")
	for _, tc := range []struct {
		req   saveRequest
		field string
	}{
		{saveRequest{ZimbraURL: "ftp://x", Port: a.cfg.ListenPort}, "url"},
		{saveRequest{ZimbraURL: "https://old.example", Port: 80}, "port"},
	} {
		res := callUI(t, a, "save", tc.req).(saveResult)
		if res.OK || res.Field != tc.field {
			t.Errorf("save(%+v) = %+v, attendu une erreur sur %q", tc.req, res, tc.field)
		}
	}
	if _, err := os.Stat(configPath()); err == nil {
		t.Errorf("config écrite malgré des champs invalides")
	}
}

func TestSaveUnreachableURLNeedsConfirmation(t *testing.T) {
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()
	a := newTestApp(t, "https://old.example")
	req := saveRequest{ZimbraURL: deadURL, Port: a.cfg.ListenPort}

	res := callUI(t, a, "save", req).(saveResult)
	if res.OK || res.Warning == "" {
		t.Fatalf("URL injoignable : %+v, attendu un avertissement", res)
	}
	if a.gw.ZimbraURL() != "https://old.example" {
		t.Errorf("URL appliquée sans confirmation")
	}

	req.Force = true
	if res := callUI(t, a, "save", req).(saveResult); !res.OK {
		t.Fatalf("save forcé : %+v", res)
	}
	if a.gw.ZimbraURL() != deadURL || loadConfig().ZimbraURL != deadURL {
		t.Errorf("URL forcée non appliquée ou non enregistrée")
	}
}

func TestSaveReachableURLAppliesDirectly(t *testing.T) {
	zs := httptest.NewTLSServer(&fakeZimbra{})
	defer zs.Close()
	orig := httpClient
	httpClient = zs.Client()
	t.Cleanup(func() { httpClient = orig })
	a := newTestApp(t, "https://old.example")
	res := callUI(t, a, "save", saveRequest{ZimbraURL: zs.URL + "/service/soap", Port: a.cfg.ListenPort}).(saveResult)
	if !res.OK || res.State.ZimbraURL != zs.URL {
		t.Fatalf("save = %+v, attendu OK avec l'URL normalisée %s", res, zs.URL)
	}
	if res.State.Zimbra != "ok" {
		t.Errorf("état Zimbra après changement d'URL = %q, attendu ok", res.State.Zimbra)
	}
}

func TestSavePortRestartsGateway(t *testing.T) {
	a := newTestApp(t, "https://old.example")
	oldPort, newPort := a.cfg.ListenPort, freePort(t)

	res := callUI(t, a, "save", saveRequest{ZimbraURL: "https://old.example", Port: newPort}).(saveResult)
	if !res.OK || !listening(newPort) || listening(oldPort) {
		t.Fatalf("changement de port : %+v, écoute nouveau=%v ancien=%v", res, listening(newPort), listening(oldPort))
	}

	// Port déjà pris : erreur sur le champ, la passerelle reste sur l'ancien port.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	res = callUI(t, a, "save", saveRequest{ZimbraURL: "https://old.example", Port: busyPort}).(saveResult)
	if res.OK || res.Field != "port" {
		t.Fatalf("port occupé : %+v, attendu une erreur sur le port", res)
	}
	if !listening(newPort) || a.cfg.ListenPort != newPort || a.portError() != "" {
		t.Errorf("la passerelle n'est pas revenue sur le port %d", newPort)
	}
}

func TestToggleGateway(t *testing.T) {
	a := newTestApp(t, "https://old.example")
	port, ldapPort := a.cfg.ListenPort, a.cfg.LDAPPort
	if s := callUI(t, a, "toggle", nil).(uiState); s.Running || s.LDAPRunning || listening(port) || listening(ldapPort) {
		t.Fatalf("toggle devait arrêter les deux serveurs : %+v", s)
	}
	if s := callUI(t, a, "toggle", nil).(uiState); !s.Running || !s.LDAPRunning || !listening(port) || !listening(ldapPort) {
		t.Fatalf("toggle devait redémarrer les deux serveurs : %+v", s)
	}
}

func TestSaveLDAPSettingsAndValidation(t *testing.T) {
	a := newTestApp(t, "https://old.example")
	smtpPort := a.cfg.ListenPort
	ldapPort := a.cfg.LDAPPort

	// Rejet port LDAP identique au port SMTP ou hors bornes.
	for _, badPort := range []int{smtpPort, 80} {
		res := callUI(t, a, "save", saveRequest{
			ZimbraURL:   "https://old.example",
			Port:        smtpPort,
			LDAPEnabled: true,
			LDAPPort:    badPort,
		}).(saveResult)
		if res.OK || res.Field != "ldapPort" {
			t.Errorf("save(ldapPort=%d) = %+v, attendu erreur sur ldapPort", badPort, res)
		}
	}

	// Désactivation LDAP.
	res := callUI(t, a, "save", saveRequest{
		ZimbraURL:   "https://old.example",
		Port:        smtpPort,
		LDAPEnabled: false,
		LDAPPort:    ldapPort,
	}).(saveResult)
	if !res.OK || res.State.LDAPRunning || listening(ldapPort) || !listening(smtpPort) {
		t.Fatalf("désactivation LDAP : %+v", res)
	}

	// Réactivation sur un nouveau port.
	ldapPort2 := freePort(t)
	res = callUI(t, a, "save", saveRequest{
		ZimbraURL:   "https://old.example",
		Port:        smtpPort,
		LDAPEnabled: true,
		LDAPPort:    ldapPort2,
	}).(saveResult)
	if !res.OK || !res.State.LDAPRunning || !listening(ldapPort2) {
		t.Fatalf("réactivation LDAP : %+v", res)
	}

	// Port LDAP déjà pris : erreur + retour sur ldapPort2.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	res = callUI(t, a, "save", saveRequest{
		ZimbraURL:   "https://old.example",
		Port:        smtpPort,
		LDAPEnabled: true,
		LDAPPort:    busyPort,
	}).(saveResult)
	if res.OK || res.Field != "ldapPort" {
		t.Fatalf("port LDAP occupé : %+v", res)
	}
	if !listening(ldapPort2) || a.cfg.LDAPPort != ldapPort2 || a.ldapPortError() != "" {
		t.Errorf("LDAP n'est pas revenu sur le port %d", ldapPort2)
	}

	// Échange des ports SMTP et LDAP.
	res = callUI(t, a, "save", saveRequest{
		ZimbraURL:   "https://old.example",
		Port:        ldapPort2,
		LDAPEnabled: true,
		LDAPPort:    smtpPort,
	}).(saveResult)
	if !res.OK || !res.State.Running || !res.State.LDAPRunning || !listening(ldapPort2) || !listening(smtpPort) {
		t.Fatalf("échange de ports SMTP/LDAP : %+v", res)
	}
}

func TestLoadConfigBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("HOME", dir)

	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), []byte(`{"zimbra_url":"https://mail.ex.fr","listen_port":2025}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig()
	if cfg.ZimbraURL != "https://mail.ex.fr" || cfg.ListenPort != 2025 || !cfg.LDAPEnabled || cfg.LDAPPort != 3890 {
		t.Errorf("rétrocompatibilité config : %+v", cfg)
	}

	if err := os.WriteFile(configPath(), []byte(`{"zimbra_url":"https://mail.ex.fr","listen_port":3890}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2 := loadConfig()
	if cfg2.LDAPPort != 3891 {
		t.Errorf("collision port LDAP/SMTP = %d, attendu 3891", cfg2.LDAPPort)
	}
}
