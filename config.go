package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const appID = "ZimbraSmtpProxy"

type Config struct {
	ZimbraURL  string `json:"zimbra_url"`
	ListenPort int    `json:"listen_port"`
}

var defaultConfig = Config{
	ZimbraURL:  "https://mail-etu.esisar.grenoble-inp.fr",
	ListenPort: 1025,
}

// dataDir renvoie %APPDATA%\ZimbraSmtpProxy (config + journal).
func dataDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, appID)
}

func configPath() string { return filepath.Join(dataDir(), "config.json") }
func logPath() string    { return filepath.Join(dataDir(), "proxy.log") }

func loadConfig() Config {
	cfg := defaultConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("lecture config: %v", err)
		}
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Printf("config invalide, valeurs par défaut utilisées: %v", err)
		return defaultConfig
	}
	if u, err := normalizeZimbraURL(cfg.ZimbraURL); err == nil {
		cfg.ZimbraURL = u
	} else {
		cfg.ZimbraURL = defaultConfig.ZimbraURL
	}
	if validPort(cfg.ListenPort) != nil {
		cfg.ListenPort = defaultConfig.ListenPort
	}
	return cfg
}

func (c Config) save() error {
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}

// normalizeZimbraURL accepte "mail.exemple.fr", "https://mail.exemple.fr/" ou
// l'URL complète du endpoint SOAP, et renvoie la racine "https://mail.exemple.fr".
func normalizeZimbraURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("URL invalide : %q", s)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/service/soap")
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

func validPort(p int) error {
	if p < 1024 || p > 65535 {
		return fmt.Errorf("le port doit être compris entre 1024 et 65535")
	}
	return nil
}
