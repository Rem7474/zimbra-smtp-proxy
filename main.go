// Zimbra SMTP Proxy : passerelle SMTP locale vers l'API SOAP Zimbra, pilotée
// depuis une icône de la zone de notification Windows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/ncruces/zenity"

	"zimbra-smtp-proxy/internal/icon"
)

const (
	appName       = "Zimbra SMTP Proxy"
	pingInterval  = 5 * time.Minute
	pingTimeout   = 10 * time.Second
	logRotateSize = 1 << 20
)

var version = "dev" // injecté au build : -X main.version=1.0.0

var (
	iconOK      = icon.ICO(icon.Green, 16, 20, 24, 32, 48)
	iconWarn    = icon.ICO(icon.Orange, 16, 20, 24, 32, 48)
	iconStopped = icon.ICO(icon.Grey, 16, 20, 24, 32, 48)
)

type zimbraState int

const (
	zUnknown zimbraState = iota
	zOK
	zKO
)

type app struct {
	gw *Gateway

	opMu sync.Mutex // sérialise les actions (menu, fenêtre)

	mu      sync.Mutex // protège les champs ci-dessous et l'icône
	cfg     Config
	zState  zimbraState
	zDetail string
	portErr string
	uiPush  func(uiState) // fenêtre de configuration ouverte, sinon nil

	mStatus, mZimbra, mOpen, mTest, mToggle, mQuit *systray.MenuItem
}

func main() {
	setupLog()
	if !singleInstance() {
		zenity.Info(appName+" est déjà lancé.\nVoir l'icône dans la zone de notification.", zenity.Title(appName))
		return
	}
	cfg := loadConfig()
	a := &app{cfg: cfg, gw: NewGateway(cfg.ZimbraURL)}
	log.Printf("démarrage %s %s", appName, version)
	systray.Run(a.onReady, a.gw.Stop)
}

func setupLog() {
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return
	}
	p := logPath()
	if fi, err := os.Stat(p); err == nil && fi.Size() > logRotateSize {
		os.Rename(p, p+".old")
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(f)
	}
}

func (a *app) onReady() {
	systray.SetTitle(appName)

	a.mStatus = systray.AddMenuItem("", "")
	a.mStatus.Disable()
	a.mZimbra = systray.AddMenuItem("", "")
	a.mZimbra.Disable()
	systray.AddSeparator()
	a.mOpen = systray.AddMenuItem("Ouvrir la configuration…", "")
	a.mTest = systray.AddMenuItem("Tester la connexion Zimbra", "")
	a.mToggle = systray.AddMenuItem("", "")
	systray.AddSeparator()
	a.mQuit = systray.AddMenuItem("Quitter", "")
	systray.SetOnTapped(a.openSettings) // clic gauche ; clic droit = menu

	startErr := a.startGateway(a.cfg.ListenPort)
	a.refresh()
	if startErr != nil {
		go errorBox("%s\n\nLe port est peut-être utilisé par un autre programme.\nChangez-le dans la fenêtre de configuration.", a.portError())
	}
	go a.menuLoop()
	go a.pingLoop()
}

func (a *app) menuLoop() {
	for {
		select {
		case <-a.mOpen.ClickedCh:
			a.openSettings()
		case <-a.mTest.ClickedCh:
			a.testFromMenu()
		case <-a.mToggle.ClickedCh:
			a.opMu.Lock()
			a.toggleGatewayLocked()
			a.opMu.Unlock()
		case <-a.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// ---------- État et icône ----------

func formatLatency(d time.Duration) string {
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// refresh met à jour l'icône, le menu et la fenêtre ouverte.
func (a *app) refresh() {
	a.mu.Lock()
	s := a.snapshotLocked()
	push := a.uiPush
	if a.mStatus != nil { // menu construit (absent dans les tests)
		a.refreshTrayLocked(s)
	}
	a.mu.Unlock()

	if push != nil {
		push(s)
	}
}

func (a *app) refreshTrayLocked(s uiState) {
	switch {
	case s.PortError != "":
		a.mStatus.SetTitle("⚠ Passerelle arrêtée : échec du démarrage")
	case !s.Running:
		a.mStatus.SetTitle("○ Passerelle arrêtée")
	default:
		a.mStatus.SetTitle("● Passerelle active sur " + s.Addr)
	}
	if s.Running {
		a.mToggle.SetTitle("Arrêter la passerelle")
	} else {
		a.mToggle.SetTitle("Démarrer la passerelle")
	}

	var zimbra string
	switch a.zState {
	case zOK:
		zimbra = "Zimbra joignable (" + a.zDetail + ")"
	case zKO:
		zimbra = "Zimbra injoignable"
	default:
		zimbra = "Zimbra : test en cours…"
	}
	a.mZimbra.SetTitle(zimbra)

	switch {
	case !s.Running:
		systray.SetIcon(iconStopped)
	case a.zState == zKO:
		systray.SetIcon(iconWarn)
	default:
		systray.SetIcon(iconOK)
	}
	status := "arrêtée"
	if s.Running {
		status = s.Addr
	}
	systray.SetTooltip(fmt.Sprintf("%s — %s\n%s", appName, status, zimbra))
}

// checkZimbra teste l'URL courante et met l'état à jour. Il renvoie l'état
// précédent pour que l'appelant puisse signaler les changements.
func (a *app) checkZimbra() (prev, cur zimbraState, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	d, err := PingZimbra(ctx, a.gw.ZimbraURL())

	a.mu.Lock()
	prev = a.zState
	if err != nil {
		a.zState, a.zDetail = zKO, err.Error()
	} else {
		a.zState, a.zDetail = zOK, formatLatency(d)
	}
	cur, detail = a.zState, a.zDetail
	a.mu.Unlock()

	a.refresh()
	return prev, cur, detail
}

func (a *app) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		prev, cur, detail := a.checkZimbra()
		if cur != prev {
			if cur == zKO {
				log.Printf("Zimbra injoignable: %s", detail)
				zenity.Notify("Zimbra injoignable : "+detail+"\nLes envois depuis Thunderbird échoueront.",
					zenity.Title(appName), zenity.WarningIcon)
			} else if prev == zKO {
				log.Printf("Zimbra de nouveau joignable")
				zenity.Notify("Zimbra est de nouveau joignable.", zenity.Title(appName), zenity.InfoIcon)
			}
		}
		<-t.C
	}
}

func (a *app) testFromMenu() {
	_, cur, detail := a.checkZimbra()
	if cur == zOK {
		zenity.Notify("Zimbra répond ("+detail+").", zenity.Title(appName), zenity.InfoIcon)
	} else {
		zenity.Notify("Zimbra ne répond pas : "+detail, zenity.Title(appName), zenity.WarningIcon)
	}
}

func errorBox(format string, args ...any) {
	err := zenity.Error(fmt.Sprintf(format, args...), zenity.Title(appName), zenity.ErrorIcon)
	if err != nil && !errors.Is(err, zenity.ErrCanceled) {
		log.Printf("dialogue: %v", err)
	}
}
