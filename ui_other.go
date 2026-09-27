//go:build !windows

package main

// La fenêtre de configuration utilise WebView2, propre à Windows.
func (a *app) openSettings() {
	errorBox("La fenêtre de configuration n'est disponible que sous Windows.\nConfiguration : %s", configPath())
}
