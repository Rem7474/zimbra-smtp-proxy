//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

// L'application cible Windows ; ces versions permettent de compiler et de
// tester la passerelle sur Linux/macOS.

func singleInstance() bool    { return true }
func autostartEnabled() bool  { return false }
func setAutostart(bool) error { return errors.New("non supporté sur cette plateforme") }
func openFile(p string) error { return exec.Command("xdg-open", p).Start() }
