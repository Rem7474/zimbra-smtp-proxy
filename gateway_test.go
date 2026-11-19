package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeZimbra simule AuthRequest, NoOpRequest, SendMsgRequest, AutoCompleteRequest et l'upload.
type fakeZimbra struct {
	mu                   sync.Mutex
	uploaded             []byte
	sentAid              string
	authCalls            int
	autoCompleteCalls    int
	lastAutoQuery        string
	tokenToIssue         string
	authLifetimeMs       int64
	expiredTokens        map[string]bool
	matches              []map[string]any
	autoCompleteWait     chan struct{}
	autoCompleteStarted  chan struct{}
	autoCompleteCanceled chan struct{}
}

func (f *fakeZimbra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/service/upload":
		c, err := r.Cookie("ZM_AUTH_TOKEN")
		if err != nil || c.Value != "tok-alice" || r.URL.Query().Get("fmt") != "raw" {
			io.WriteString(w, "401,'null'")
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.uploaded = b
		f.mu.Unlock()
		io.WriteString(w, "200,'null','aid-42'")
	case "/service/soap":
		var req struct {
			Header struct {
				Context struct {
					AuthToken *struct {
						Content string `json:"_content"`
					} `json:"authToken"`
				} `json:"context"`
			}
			Body map[string]json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&req)
		fault := func(code, msg string) {
			w.WriteHeader(500)
			fObj := map[string]any{"Reason": map[string]any{"Text": msg}}
			if code != "" {
				fObj["Detail"] = map[string]any{"Error": map[string]any{"Code": code}}
			}
			json.NewEncoder(w).Encode(map[string]any{"Body": map[string]any{"Fault": fObj}})
		}
		switch {
		case req.Body["AuthRequest"] != nil:
			var ar struct {
				Account struct {
					Content string `json:"_content"`
				} `json:"account"`
				Password struct {
					Content string `json:"_content"`
				} `json:"password"`
			}
			json.Unmarshal(req.Body["AuthRequest"], &ar)
			if ar.Password.Content != "secret" {
				fault("account.AUTH_FAILED", "authentication failed for ["+ar.Account.Content+"]")
				return
			}
			f.mu.Lock()
			f.authCalls++
			tok := f.tokenToIssue
			if tok == "" {
				tok = "tok-alice"
			}
			life := f.authLifetimeMs
			f.mu.Unlock()
			resp := map[string]any{"authToken": []map[string]any{{"_content": tok}}}
			if life > 0 {
				resp["lifetime"] = life
			}
			json.NewEncoder(w).Encode(map[string]any{"Body": map[string]any{"AuthResponse": resp}})

		case req.Body["SendMsgRequest"] != nil:
			if req.Header.Context.AuthToken == nil || req.Header.Context.AuthToken.Content != "tok-alice" {
				fault("service.AUTH_REQUIRED", "no valid authtoken present")
				return
			}
			var m struct {
				M struct{ Aid string } `json:"m"`
			}
			json.Unmarshal(req.Body["SendMsgRequest"], &m)
			f.mu.Lock()
			f.sentAid = m.M.Aid
			f.mu.Unlock()
			io.WriteString(w, `{"Body":{"SendMsgResponse":{"m":[{"id":"1"}]}}}`)

		case req.Body["AutoCompleteRequest"] != nil:
			var tok string
			if req.Header.Context.AuthToken != nil {
				tok = req.Header.Context.AuthToken.Content
			}
			f.mu.Lock()
			expired := f.expiredTokens[tok]
			expected := f.tokenToIssue
			if expected == "" {
				expected = "tok-alice"
			}
			waitCh := f.autoCompleteWait
			startedCh := f.autoCompleteStarted
			canceledCh := f.autoCompleteCanceled
			matches := f.matches
			f.mu.Unlock()

			if tok == "" {
				fault("service.AUTH_REQUIRED", "no valid authtoken present")
				return
			}
			if expired {
				fault("service.AUTH_EXPIRED", "auth credentials have expired")
				return
			}
			if tok != expected && tok != "tok-alice" {
				fault("service.AUTH_REQUIRED", "no valid authtoken present")
				return
			}
			if waitCh != nil {
				if startedCh != nil {
					select {
					case startedCh <- struct{}{}:
					default:
					}
				}
				select {
				case <-waitCh:
				case <-r.Context().Done():
					if canceledCh != nil {
						select {
						case canceledCh <- struct{}{}:
						default:
						}
					}
					return
				}
			}

			var ac struct {
				Name       string `json:"name"`
				IncludeGal int    `json:"includeGal"`
			}
			json.Unmarshal(req.Body["AutoCompleteRequest"], &ac)
			f.mu.Lock()
			f.autoCompleteCalls++
			f.lastAutoQuery = ac.Name
			f.mu.Unlock()

			if matches == nil {
				matches = []map[string]any{
					{"email": `"Jean Dupont" <jean.dupont@ex.fr>`, "display": "Jean Dupont", "first": "Jean", "last": "Dupont"},
					{"email": "alice.durand@ex.fr", "display": "Alice Durand"},
					{"email": `"Jean Dupont" <jean.dupont@ex.fr>`, "display": "Doublon"},
					{"email": "groupe-multi: a@ex.fr, b@ex.fr", "display": "Groupe"},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Body": map[string]any{
					"AutoCompleteResponse": map[string]any{
						"match": matches,
					},
				},
			})

		default:
			fault("", "no valid authtoken present")
		}
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startGateway(t *testing.T, zimbraURL string) string {
	t.Helper()
	gw := NewGateway(zimbraURL)
	port := freePort(t)
	if err := gw.Start(port); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Stop)
	return "127.0.0.1:" + strconv.Itoa(port)
}

const testMsg = "From: alice@ex.fr\r\nTo: bob@ex.fr\r\nCc: carol@ex.fr\r\nSubject: Test\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n" +
	"--XX\r\nContent-Type: text/plain\r\n\r\nBonjour\r\n" +
	"--XX\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\n\r\nJVBERi0=\r\n--XX--\r\n"

func TestSendRawMIMEWithBcc(t *testing.T) {
	fz := &fakeZimbra{}
	zs := httptest.NewServer(fz)
	defer zs.Close()
	addr := startGateway(t, zs.URL)

	auth := smtp.PlainAuth("", "alice@ex.fr", "secret", "127.0.0.1")
	err := smtp.SendMail(addr, auth, "alice@ex.fr", []string{"bob@ex.fr", "carol@ex.fr", "dave@ex.fr"}, []byte(testMsg))
	if err != nil {
		t.Fatalf("SendMail: %v", err)
	}
	if fz.sentAid != "aid-42" {
		t.Errorf("SendMsgRequest aid = %q, attendu aid-42", fz.sentAid)
	}
	up := string(fz.uploaded)
	if !strings.HasPrefix(up, "Bcc: dave@ex.fr\r\n") {
		t.Errorf("Bcc manquant pour le destinataire caché:\n%.120s", up)
	}
	if !strings.Contains(up, "filename=a.pdf") || !strings.Contains(up, "Cc: carol@ex.fr") {
		t.Errorf("le MIME d'origine (pièce jointe, Cc) n'est pas transmis intact")
	}
}

func TestNoBccHeaderWhenAllRecipientsVisible(t *testing.T) {
	got := addMissingBcc([]byte(testMsg), []string{"BOB@ex.fr", "carol@ex.fr"})
	if string(got) != testMsg {
		t.Errorf("message modifié alors que tous les destinataires sont visibles")
	}
}

func TestAuthFailures(t *testing.T) {
	zs := httptest.NewServer(&fakeZimbra{})
	addr := startGateway(t, zs.URL)

	err := smtp.SendMail(addr, smtp.PlainAuth("", "alice@ex.fr", "wrong", "127.0.0.1"), "alice@ex.fr", []string{"bob@ex.fr"}, []byte(testMsg))
	if err == nil || !strings.HasPrefix(err.Error(), "535") {
		t.Errorf("mauvais mot de passe: erreur = %v, attendu 535", err)
	}

	// Zimbra coupé : erreur temporaire (4xx) et non « mot de passe invalide ».
	zs.Close()
	err = smtp.SendMail(addr, smtp.PlainAuth("", "alice@ex.fr", "secret", "127.0.0.1"), "alice@ex.fr", []string{"bob@ex.fr"}, []byte(testMsg))
	if err == nil || !strings.HasPrefix(err.Error(), "454") {
		t.Errorf("Zimbra injoignable: erreur = %v, attendu 454", err)
	}
}

func TestPingZimbra(t *testing.T) {
	zs := httptest.NewServer(&fakeZimbra{})
	if _, err := PingZimbra(context.Background(), zs.URL); err != nil {
		t.Errorf("serveur qui répond par un Fault SOAP: %v, attendu joignable", err)
	}
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "<html>portail captif</html>")
	}))
	defer html.Close()
	if _, err := PingZimbra(context.Background(), html.URL); err == nil {
		t.Errorf("une page HTML ne doit pas être considérée comme Zimbra")
	}
	zs.Close()
	if _, err := PingZimbra(context.Background(), zs.URL); err == nil {
		t.Errorf("serveur arrêté considéré comme joignable")
	}
}

func TestNormalizeZimbraURL(t *testing.T) {
	cases := map[string]string{
		"mail.ex.fr":                           "https://mail.ex.fr",
		" https://mail.ex.fr/ ":                "https://mail.ex.fr",
		"https://mail.ex.fr/service/soap":      "https://mail.ex.fr",
		"http://mail.ex.fr:8080/service/soap/": "http://mail.ex.fr:8080",
	}
	for in, want := range cases {
		if got, err := normalizeZimbraURL(in); err != nil || got != want {
			t.Errorf("normalizeZimbraURL(%q) = %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://mail.ex.fr", "https://"} {
		if _, err := normalizeZimbraURL(bad); err == nil {
			t.Errorf("normalizeZimbraURL(%q) aurait dû échouer", bad)
		}
	}
}
