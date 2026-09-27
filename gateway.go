package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const maxMsgSize = 25 << 20 // 25 Mo

var httpClient = &http.Client{Timeout: 60 * time.Second}

// Gateway est le serveur SMTP local qui relaie vers l'API SOAP Zimbra.
type Gateway struct {
	zimbraURL atomic.Value // string

	mu   sync.Mutex
	srv  *smtp.Server
	ln   net.Listener
	addr string
}

func NewGateway(zimbraURL string) *Gateway {
	g := &Gateway{}
	g.zimbraURL.Store(zimbraURL)
	return g
}

func (g *Gateway) ZimbraURL() string     { return g.zimbraURL.Load().(string) }
func (g *Gateway) SetZimbraURL(u string) { g.zimbraURL.Store(u) }

// Start écoute sur 127.0.0.1:port. L'erreur (port occupé...) est renvoyée
// immédiatement, avant de lancer la boucle de service.
func (g *Gateway) Start(port int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srv != nil {
		return nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := smtp.NewServer(&backend{gw: g})
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true // écoute uniquement en local
	srv.MaxMessageBytes = maxMsgSize
	srv.ReadTimeout = 2 * time.Minute
	srv.WriteTimeout = 2 * time.Minute
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, smtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("serveur SMTP: %v", err)
		}
	}()
	g.srv, g.ln, g.addr = srv, l, addr
	log.Printf("passerelle active sur %s -> %s", addr, g.ZimbraURL())
	return nil
}

// Stop laisse 10 s aux envois en cours pour se terminer, puis coupe.
func (g *Gateway) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srv == nil {
		return
	}
	// Fermé ici : Serve n'a peut-être pas encore enregistré le listener
	// auprès du serveur si Stop suit immédiatement Start.
	g.ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.srv.Shutdown(ctx); err != nil {
		g.srv.Close()
	}
	g.srv, g.ln = nil, nil
	log.Printf("passerelle arrêtée")
}

// Status renvoie l'adresse d'écoute, vide si la passerelle est arrêtée.
func (g *Gateway) Status() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srv == nil {
		return ""
	}
	return g.addr
}

// ---------- Client Zimbra ----------

type soapFault struct{ msg string }

func (f *soapFault) Error() string { return "zimbra: " + f.msg }

func soapCall(ctx context.Context, base, token string, body map[string]any) (map[string]any, error) {
	hdr := map[string]any{"_jsns": "urn:zimbra"}
	if token != "" {
		hdr["authToken"] = map[string]any{"_content": token}
	}
	payload, _ := json.Marshal(map[string]any{
		"Header": map[string]any{"context": hdr},
		"Body":   body,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/service/soap", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Body map[string]any `json:"Body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Body == nil {
		return nil, fmt.Errorf("réponse non-Zimbra (HTTP %d)", resp.StatusCode)
	}
	if f, ok := out.Body["Fault"].(map[string]any); ok {
		msg := "erreur inconnue"
		if r, ok := f["Reason"].(map[string]any); ok {
			msg, _ = r["Text"].(string)
		}
		return nil, &soapFault{msg}
	}
	return out.Body, nil
}

// PingZimbra vérifie que l'API SOAP répond. Sans jeton, Zimbra renvoie une
// erreur « auth required » : c'est la preuve attendue qu'il est joignable.
func PingZimbra(ctx context.Context, base string) (time.Duration, error) {
	t0 := time.Now()
	_, err := soapCall(ctx, base, "", map[string]any{
		"NoOpRequest": map[string]any{"_jsns": "urn:zimbraMail"},
	})
	var f *soapFault
	if err == nil || errors.As(err, &f) {
		return time.Since(t0), nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return 0, err
}

func zimbraAuth(base, user, pass string) (string, error) {
	body, err := soapCall(context.Background(), base, "", map[string]any{"AuthRequest": map[string]any{
		"_jsns":    "urn:zimbraAccount",
		"account":  map[string]any{"_content": user, "by": "name"},
		"password": map[string]any{"_content": pass},
	}})
	if err != nil {
		return "", err
	}
	ar, _ := body["AuthResponse"].(map[string]any)
	tokens, _ := ar["authToken"].([]any)
	if len(tokens) == 0 {
		return "", errors.New("pas de authToken dans la réponse")
	}
	t, _ := tokens[0].(map[string]any)["_content"].(string)
	return t, nil
}

var aidRe = regexp.MustCompile(`'([^']*)'\s*$`)

// uploadMIME envoie le message RFC 822 brut au FileUploadServlet et renvoie son aid.
func uploadMIME(base, token string, raw []byte) (string, error) {
	req, _ := http.NewRequest("POST", base+"/service/upload?fmt=raw", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("Content-Disposition", `attachment; filename="message.eml"`)
	req.AddCookie(&http.Cookie{Name: "ZM_AUTH_TOKEN", Value: token})
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	// Réponse attendue : 200,'null','<aid>'
	if !strings.HasPrefix(string(b), "200,") {
		return "", fmt.Errorf("upload refusé: %.200s", b)
	}
	m := aidRe.FindSubmatch(b)
	if m == nil || len(m[1]) == 0 {
		return "", fmt.Errorf("aid introuvable: %.200s", b)
	}
	return string(m[1]), nil
}

func zimbraSendRaw(base, token string, raw []byte) error {
	aid, err := uploadMIME(base, token, raw)
	if err != nil {
		return err
	}
	_, err = soapCall(context.Background(), base, token, map[string]any{"SendMsgRequest": map[string]any{
		"_jsns": "urn:zimbraMail",
		"m":     map[string]any{"aid": aid},
	}})
	return err
}

// addMissingBcc ajoute un en-tête Bcc pour les destinataires de l'enveloppe
// absents de To/Cc (Thunderbird retire le Bcc du message avant DATA).
func addMissingBcc(raw []byte, rcpts []string) []byte {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	known := map[string]bool{}
	for _, h := range []string{"To", "Cc", "Bcc"} {
		list, _ := msg.Header.AddressList(h)
		for _, a := range list {
			known[strings.ToLower(a.Address)] = true
		}
	}
	var missing []string
	for _, r := range rcpts {
		if !known[strings.ToLower(r)] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return raw
	}
	return append([]byte("Bcc: "+strings.Join(missing, ", ")+"\r\n"), raw...)
}

// ---------- Session SMTP ----------

type backend struct{ gw *Gateway }

func (b *backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &session{gw: b.gw}, nil
}

type session struct {
	gw          *Gateway
	user, token string
	rcpts       []string
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		token, err := zimbraAuth(s.gw.ZimbraURL(), user, pass)
		if err != nil {
			log.Printf("auth %s refusée: %v", user, err)
			var f *soapFault
			if errors.As(err, &f) {
				return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "Authentication credentials invalid"}
			}
			return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Zimbra unreachable"}
		}
		s.user, s.token = user, token
		return nil
	}), nil
}

func (s *session) Mail(string, *smtp.MailOptions) error {
	if s.token == "" {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := zimbraSendRaw(s.gw.ZimbraURL(), s.token, addMissingBcc(raw, s.rcpts)); err != nil {
		log.Printf("envoi %s: %v", s.user, err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Zimbra API error"}
	}
	log.Printf("mail de %s envoyé à %s", s.user, strings.Join(s.rcpts, ", "))
	return nil
}

func (s *session) Reset()        { s.rcpts = nil }
func (s *session) Logout() error { return nil }
