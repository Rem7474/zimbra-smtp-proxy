package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui/settings.html
var settingsHTML string

const webview2Download = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// settingsWin : au plus une fenêtre de configuration à la fois.
var settingsWin struct {
	sync.Mutex
	hwnd uintptr
	open bool
}

// openSettings ouvre la fenêtre ou la ramène au premier plan. Non bloquant :
// appelé depuis le thread de la zone de notification.
func (a *app) openSettings() {
	settingsWin.Lock()
	defer settingsWin.Unlock()
	if settingsWin.open {
		if settingsWin.hwnd != 0 {
			const swRestore = 9
			procShowWindow.Call(settingsWin.hwnd, swRestore)
			procSetForegroundWindow.Call(settingsWin.hwnd)
		}
		return
	}
	settingsWin.open = true
	go a.runSettingsWindow()
}

func dpiScale() float64 {
	if procGetDpiForSystem.Find() != nil {
		return 1
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi == 0 {
		return 1
	}
	return float64(dpi) / 96
}

// runSettingsWindow crée la fenêtre WebView2 et exécute sa boucle de messages
// sur un thread système dédié jusqu'à sa fermeture.
func (a *app) runSettingsWindow() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// WebView2 exige COM en mode STA sur le thread qui crée la fenêtre.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err == nil {
		defer windows.CoUninitialize()
	}
	defer func() {
		settingsWin.Lock()
		settingsWin.open, settingsWin.hwnd = false, 0
		settingsWin.Unlock()
	}()

	scale := dpiScale()
	height := uint(700 * scale)
	const smCyFullscreen = 17 // hauteur de la zone de travail, hors barre des tâches
	if screen, _, _ := procGetSystemMetrics.Call(smCyFullscreen); screen > 0 && height > uint(screen) {
		height = uint(screen)
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(dataDir(), "webview"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  appName,
			Width:  uint(560 * scale),
			Height: height,
			IconId: 1, // icône de l'exe (go-winres)
			Center: true,
		},
	})
	if w == nil {
		log.Printf("WebView2 indisponible")
		errorBox("La fenêtre de configuration nécessite Microsoft Edge WebView2 Runtime.\n\nTéléchargement : %s", webview2Download)
		return
	}
	w.SetSize(int(440*scale), int(480*scale), webview2.HintMin)

	settingsWin.Lock()
	settingsWin.hwnd = uintptr(w.Window())
	settingsWin.Unlock()

	eval := func(js string) { w.Dispatch(func() { w.Eval(js) }) }

	// Les appels JS sont traités hors du thread de l'interface pour qu'un test
	// réseau ne fige pas la fenêtre ; la réponse revient par __goDone.
	w.Bind("goCall", func(id int, method string, arg json.RawMessage) {
		if method == "close" {
			w.Dispatch(w.Destroy)
			return
		}
		go func() {
			b, _ := json.Marshal(a.handleUI(method, arg))
			eval(fmt.Sprintf("window.__goDone(%d, %s)", id, b))
		}()
	})

	a.mu.Lock()
	a.uiPush = func(s uiState) {
		b, _ := json.Marshal(s)
		eval(fmt.Sprintf("window.__goState && window.__goState(%s)", b))
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.uiPush = nil
		a.mu.Unlock()
	}()

	w.SetHtml(settingsHTML)
	w.Run()
}
