package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// uiState est l'état affiché par la fenêtre de configuration.
type uiState struct {
	Running      bool   `json:"running"`
	Addr         string `json:"addr"`
	PortError    string `json:"portError"`
	ZimbraURL    string `json:"zimbraURL"`
	Port         int    `json:"port"`
	Zimbra       string `json:"zimbra"` // unknown | ok | ko
	ZimbraDetail string `json:"zimbraDetail"`
	Autostart    bool   `json:"autostart"`
	Version      string `json:"version"`
}

type testResult struct {
	OK     bool   `json:"ok"`
	URL    string `json:"url"`
	Detail string `json:"detail"`
}

type saveRequest struct {
	ZimbraURL string `json:"zimbraURL"`
	Port      int    `json:"port"`
	Autostart bool   `json:"autostart"`
	Force     bool   `json:"force"` // enregistrer malgré les avertissements
}

type saveResult struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Field   string   `json:"field,omitempty"` // champ en erreur : url | port
	Warning string   `json:"warning,omitempty"`
	State   *uiState `json:"state,omitempty"`
}

// handleUI traite un appel de la fenêtre. Les actions sont sérialisées avec
// celles du menu par opMu.
func (a *app) handleUI(method string, arg json.RawMessage) any {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	switch method {
	case "state":
		log.Printf("fenêtre de configuration chargée")
		return a.snapshot()
	case "toggle":
		a.toggleGatewayLocked()
		return a.snapshot()
	case "test":
		var url string
		json.Unmarshal(arg, &url)
		return a.testURL(url)
	case "save":
		var req saveRequest
		if err := json.Unmarshal(arg, &req); err != nil {
			return saveResult{Error: "requête invalide"}
		}
		return a.save(req)
	case "openLog":
		if err := openFile(logPath()); err != nil {
			return err.Error()
		}
		return ""
	}
	return nil
}

func (a *app) snapshot() uiState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

func (a *app) snapshotLocked() uiState {
	addr := a.gw.Status()
	return uiState{
		Running:      addr != "",
		Addr:         addr,
		PortError:    a.portErr,
		ZimbraURL:    a.cfg.ZimbraURL,
		Port:         a.cfg.ListenPort,
		Zimbra:       [...]string{"unknown", "ok", "ko"}[a.zState],
		ZimbraDetail: a.zDetail,
		Autostart:    autostartEnabled(),
		Version:      version,
	}
}

func (a *app) testURL(raw string) testResult {
	u, err := normalizeZimbraURL(raw)
	if err != nil {
		return testResult{Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	d, err := PingZimbra(ctx, u)
	if err != nil {
		return testResult{URL: u, Detail: err.Error()}
	}
	return testResult{OK: true, URL: u, Detail: formatLatency(d)}
}

func (a *app) save(req saveRequest) saveResult {
	u, err := normalizeZimbraURL(req.ZimbraURL)
	if err != nil {
		return saveResult{Error: err.Error(), Field: "url"}
	}
	if err := validPort(req.Port); err != nil {
		return saveResult{Error: err.Error(), Field: "port"}
	}

	a.mu.Lock()
	old := a.cfg
	a.mu.Unlock()

	if u != old.ZimbraURL && !req.Force {
		var warns []string
		if strings.HasPrefix(u, "http://") {
			warns = append(warns, "Cette adresse n'est pas en HTTPS : vos mots de passe circuleront en clair.")
		}
		if r := a.testURL(u); !r.OK {
			warns = append(warns, fmt.Sprintf("%s ne répond pas comme un serveur Zimbra (%s).", u, r.Detail))
		}
		if len(warns) > 0 {
			return saveResult{Warning: strings.Join(warns, "\n"), Field: "url"}
		}
	}

	// Port : redémarre la passerelle si elle tournait (ou était en échec).
	if req.Port != old.ListenPort || a.portErrSet() {
		wasActive := a.gw.Status() != "" || a.portErrSet()
		if wasActive {
			a.gw.Stop()
			if err := a.startGateway(req.Port); err != nil {
				msg := a.portError()
				if req.Port != old.ListenPort {
					a.startGateway(old.ListenPort)
				}
				a.refresh()
				return saveResult{Error: msg, Field: "port"}
			}
		}
	}

	if req.Autostart != autostartEnabled() {
		if err := setAutostart(req.Autostart); err != nil {
			return saveResult{Error: "Démarrage automatique : " + err.Error()}
		}
	}

	a.mu.Lock()
	a.cfg.ZimbraURL, a.cfg.ListenPort = u, req.Port
	cfg := a.cfg
	a.mu.Unlock()
	a.gw.SetZimbraURL(u)
	if err := cfg.save(); err != nil {
		return saveResult{Error: "Enregistrement impossible : " + err.Error()}
	}
	log.Printf("configuration enregistrée: %s, port %d", u, req.Port)

	if u != old.ZimbraURL {
		a.checkZimbra()
	} else {
		a.refresh()
	}
	s := a.snapshot()
	return saveResult{OK: true, State: &s}
}

// startGateway démarre la passerelle et mémorise l'erreur éventuelle pour
// l'afficher dans le menu et la fenêtre.
func (a *app) startGateway(port int) error {
	err := a.gw.Start(port)
	a.mu.Lock()
	if err != nil {
		a.portErr = fmt.Sprintf("Impossible d'écouter sur le port %d : %v", port, err)
		log.Printf("écoute port %d: %v", port, err)
	} else {
		a.portErr = ""
	}
	a.mu.Unlock()
	return err
}

func (a *app) portError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.portErr
}

func (a *app) portErrSet() bool { return a.portError() != "" }

func (a *app) toggleGatewayLocked() {
	if a.gw.Status() != "" {
		a.gw.Stop()
	} else {
		a.mu.Lock()
		port := a.cfg.ListenPort
		a.mu.Unlock()
		a.startGateway(port)
	}
	a.refresh()
}
