package main

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// singleInstance empêche deux icônes / deux écoutes sur le même port.
func singleInstance() bool {
	name, _ := windows.UTF16PtrFromString(`Local\` + appID)
	// Le handle n'est jamais fermé : le mutex vit autant que le processus.
	_, err := windows.CreateMutex(nil, false, name)
	return !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

// Démarrage automatique via HKCU\...\Run (même valeur que l'installeur).
func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appID)
	return err == nil
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return k.SetStringValue(appID, `"`+exe+`"`)
	}
	if err := k.DeleteValue(appID); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func openFile(path string) error { return exec.Command("notepad.exe", path).Start() }
