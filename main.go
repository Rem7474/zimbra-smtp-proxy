// Zimbra SMTP Proxy : passerelle SMTP locale vers l'API SOAP Zimbra, pilotée
// depuis une icône de la zone de notification Windows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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
	cfg Config // modifié uniquement par la boucle de menu
	gw  *Gateway

	mu      sync.Mutex // protège zState/zDetail et les mises à jour de l'icône
	zState  zimbraState
	zDetail string

	mStatus, mZimbra, mTest, mToggle     *systray.MenuItem
	mURL, mPort, mAutostart              *systray.MenuItem
	mThunderbird, mLog, mQuit, mSettings *systray.MenuItem
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
	a.mTest = systray.AddMenuItem("Tester la connexion Zimbra", "")
	a.mToggle = systray.AddMenuItem("", "")
	systray.AddSeparator()
	a.mSettings = systray.AddMenuItem("Paramètres", "")
	a.mURL = a.mSettings.AddSubMenuItem("URL du serveur Zimbra…", "")
	a.mPort = a.mSettings.AddSubMenuItem("Port d'écoute local…", "")
	a.mAutostart = a.mSettings.AddSubMenuItemCheckbox("Lancer à l'ouverture de session", "", autostartEnabled())
	a.mThunderbird = systray.AddMenuItem("Configurer Thunderbird…", "")
	a.mLog = systray.AddMenuItem("Ouvrir le journal", "")
	systray.AddSeparator()
	a.mQuit = systray.AddMenuItem("Quitter", "")

	a.refresh()
	go a.menuLoop()
	go a.pingLoop()
}

// menuLoop traite les clics un par un : les boîtes de dialogue sont modales
// et la config n'est jamais modifiée en parallèle.
func (a *app) menuLoop() {
	if err := a.gw.Start(a.cfg.ListenPort); err != nil {
		a.refresh()
		a.portError(err)
	}
	a.refresh()
	for {
		select {
		case <-a.mTest.ClickedCh:
			a.testZimbra()
		case <-a.mToggle.ClickedCh:
			a.toggleGateway()
		case <-a.mURL.ClickedCh:
			a.editURL()
		case <-a.mPort.ClickedCh:
			a.editPort()
		case <-a.mAutostart.ClickedCh:
			a.toggleAutostart()
		case <-a.mThunderbird.ClickedCh:
			a.showThunderbirdHelp()
		case <-a.mLog.ClickedCh:
			if err := openFile(logPath()); err != nil {
				errorBox("Impossible d'ouvrir le journal : %v", err)
			}
		case <-a.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// ---------- État et icône ----------

func (a *app) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()

	addr := a.gw.Status()
	if addr == "" {
		a.mStatus.SetTitle("○ Passerelle arrêtée")
		a.mToggle.SetTitle("Démarrer la passerelle")
	} else {
		a.mStatus.SetTitle("● Passerelle active sur " + addr)
		a.mToggle.SetTitle("Arrêter la passerelle")
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
	case addr == "":
		systray.SetIcon(iconStopped)
	case a.zState == zKO:
		systray.SetIcon(iconWarn)
	default:
		systray.SetIcon(iconOK)
	}
	status := "arrêtée"
	if addr != "" {
		status = addr
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
		a.zState, a.zDetail = zOK, d.Round(time.Millisecond).String()
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

// ---------- Actions du menu ----------

func (a *app) testZimbra() {
	_, cur, detail := a.checkZimbra()
	url := a.gw.ZimbraURL()
	if cur == zOK {
		zenity.Info(fmt.Sprintf("Zimbra répond.\n\n%s\nTemps de réponse : %s", url, detail),
			zenity.Title(appName), zenity.InfoIcon)
	} else {
		errorBox("Zimbra ne répond pas.\n\n%s\n%s", url, detail)
	}
}

func (a *app) toggleGateway() {
	if a.gw.Status() != "" {
		a.gw.Stop()
	} else if err := a.gw.Start(a.cfg.ListenPort); err != nil {
		a.portError(err)
	}
	a.refresh()
}

func (a *app) editURL() {
	s, err := zenity.Entry("URL du serveur Zimbra :", zenity.Title(appName), zenity.EntryText(a.cfg.ZimbraURL))
	if err != nil {
		return // annulé
	}
	u, err := normalizeZimbraURL(s)
	if err != nil {
		errorBox("%v", err)
		return
	}
	if u == a.cfg.ZimbraURL {
		return
	}
	if strings.HasPrefix(u, "http://") && zenity.Question(
		"Cette URL n'est pas en HTTPS : vos mots de passe circuleront en clair.\n\nContinuer quand même ?",
		zenity.Title(appName), zenity.WarningIcon, zenity.DefaultCancel()) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	_, perr := PingZimbra(ctx, u)
	cancel()
	if perr != nil && zenity.Question(
		fmt.Sprintf("%s ne répond pas comme un serveur Zimbra :\n%v\n\nEnregistrer quand même ?", u, perr),
		zenity.Title(appName), zenity.WarningIcon, zenity.DefaultCancel()) != nil {
		return
	}

	a.cfg.ZimbraURL = u
	a.gw.SetZimbraURL(u)
	a.saveConfig()
	log.Printf("URL Zimbra changée: %s", u)
	a.checkZimbra()
}

func (a *app) editPort() {
	s, err := zenity.Entry("Port d'écoute local (1024-65535) :", zenity.Title(appName),
		zenity.EntryText(strconv.Itoa(a.cfg.ListenPort)))
	if err != nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(s))
	if err == nil {
		err = validPort(port)
	}
	if err != nil {
		errorBox("Port invalide : %s", strings.TrimSpace(s))
		return
	}
	if port == a.cfg.ListenPort {
		return
	}
	if a.gw.Status() != "" {
		old := a.cfg.ListenPort
		a.gw.Stop()
		if err := a.gw.Start(port); err != nil {
			a.gw.Start(old)
			a.refresh()
			a.portErrorAt(port, err)
			return
		}
	}
	a.cfg.ListenPort = port
	a.saveConfig()
	a.refresh()
	zenity.Info(fmt.Sprintf("Port changé en %d.\nPensez à mettre à jour le serveur sortant dans Thunderbird.", port),
		zenity.Title(appName), zenity.InfoIcon)
}

func (a *app) toggleAutostart() {
	on := !a.mAutostart.Checked()
	if err := setAutostart(on); err != nil {
		errorBox("Impossible de modifier le démarrage automatique : %v", err)
		return
	}
	if on {
		a.mAutostart.Check()
	} else {
		a.mAutostart.Uncheck()
	}
}

func (a *app) showThunderbirdHelp() {
	zenity.Info(fmt.Sprintf(`Paramètres du compte → Serveur sortant (SMTP) → Ajouter :

  Nom d'hôte : 127.0.0.1
  Port : %d
  Sécurité de la connexion : Aucune
  Méthode d'authentification : Mot de passe normal
  Nom d'utilisateur : votre adresse Zimbra complète

Puis, dans « Copies et dossiers » du compte, décochez
« Placer une copie dans Envoyés » : Zimbra enregistre déjà
le message dans vos Envoyés.`, a.cfg.ListenPort),
		zenity.Title("Configurer Thunderbird"), zenity.InfoIcon)
}

// ---------- Utilitaires ----------

func (a *app) saveConfig() {
	if err := a.cfg.save(); err != nil {
		errorBox("Impossible d'enregistrer la configuration : %v", err)
	}
}

func (a *app) portError(err error) { a.portErrorAt(a.cfg.ListenPort, err) }

func (a *app) portErrorAt(port int, err error) {
	log.Printf("écoute port %d: %v", port, err)
	errorBox("Impossible d'écouter sur le port %d :\n%v\n\nLe port est peut-être utilisé par un autre programme.\nChangez-le dans Paramètres → Port d'écoute local.", port, err)
}

func errorBox(format string, args ...any) {
	err := zenity.Error(fmt.Sprintf(format, args...), zenity.Title(appName), zenity.ErrorIcon)
	if err != nil && !errors.Is(err, zenity.ErrCanceled) {
		log.Printf("dialogue: %v", err)
	}
}
