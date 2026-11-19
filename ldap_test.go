package main

import (
	"net"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

func startTestLDAP(t *testing.T, zimbraURL string) (*LDAPServer, string) {
	t.Helper()
	srv := NewLDAPServer(zimbraURL)
	port := freePort(t)
	if err := srv.Start(port); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv, "127.0.0.1:" + strconv.Itoa(port)
}

func dialTestLDAP(t *testing.T, addr string) *ldap.Conn {
	t.Helper()
	conn, err := ldap.DialURL("ldap://" + addr)
	if err != nil {
		t.Fatalf("DialURL: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestLDAPBindAndAutoComplete(t *testing.T) {
	fz := &fakeZimbra{
		matches: []map[string]any{
			{"email": `"Jean Dupont" <jean.dupont@ex.fr>`, "display": "Jean Dupont", "first": "Jean", "last": "Dupont"},
			{"email": "alice.durand@ex.fr", "display": "Alice Durand"},
			{"email": "François Éloi <f.eloi@ex.fr>"},
			{"email": `"Jean Dupont" <jean.dupont@ex.fr>`, "display": "Doublon"},
			{"email": "groupe-multi: a@ex.fr, b@ex.fr", "display": "Groupe"},
		},
	}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)
	conn := dialTestLDAP(t, addr)

	if err := conn.Bind("alice@ex.fr", "secret"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	req := ldap.NewSearchRequest(
		ldapDefaultBaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dup*)(mail=*dup*)(sn=*dup*)(givenName=*dup*))",
		nil, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(res.Entries))
	}

	e0 := res.Entries[0]
	if e0.DN != "mail=jean.dupont@ex.fr,dc=zimbra,dc=local" {
		t.Errorf("DN = %q", e0.DN)
	}
	if got := e0.GetAttributeValue("mail"); got != "jean.dupont@ex.fr" {
		t.Errorf("mail = %q", got)
	}
	if got := e0.GetAttributeValue("cn"); got != "Jean Dupont" {
		t.Errorf("cn = %q", got)
	}
	if got := e0.GetAttributeValue("displayName"); got != "Jean Dupont" {
		t.Errorf("displayName = %q", got)
	}
	if got := e0.GetAttributeValue("givenName"); got != "Jean" {
		t.Errorf("givenName = %q", got)
	}
	if got := e0.GetAttributeValue("sn"); got != "Dupont" {
		t.Errorf("sn = %q", got)
	}
	if !slices.Contains(e0.GetAttributeValues("objectClass"), "inetOrgPerson") {
		t.Errorf("objectClass = %v", e0.GetAttributeValues("objectClass"))
	}

	e1 := res.Entries[1]
	if got := e1.GetAttributeValue("givenName"); got != "Alice" {
		t.Errorf("Alice givenName = %q", got)
	}
	if got := e1.GetAttributeValue("sn"); got != "Durand" {
		t.Errorf("Alice sn = %q", got)
	}

	e2 := res.Entries[2]
	if got := e2.GetAttributeValue("mail"); got != "f.eloi@ex.fr" {
		t.Errorf("UTF-8 contact mail = %q", got)
	}
	if got := e2.GetAttributeValue("cn"); got != "François Éloi" {
		t.Errorf("UTF-8 contact cn = %q", got)
	}

	fz.mu.Lock()
	calls := fz.autoCompleteCalls
	lastQ := fz.lastAutoQuery
	fz.mu.Unlock()
	if calls != 1 || lastQ != "dup" {
		t.Errorf("calls=%d lastQ=%q, want 1 / %q", calls, lastQ, "dup")
	}

	// Deuxième requête identique : doit venir du cache mémoire.
	res2, err := conn.Search(req)
	if err != nil || len(res2.Entries) != 3 {
		t.Fatalf("Search cache hit: %v, len=%d", err, len(res2.Entries))
	}
	fz.mu.Lock()
	calls2 := fz.autoCompleteCalls
	fz.mu.Unlock()
	if calls2 != 1 {
		t.Errorf("cache non utilisé : autoCompleteCalls = %d, attendu 1", calls2)
	}

	// Filtrage d'attributs demandés (alias Thunderbird commonname -> cn).
	reqAttrs := ldap.NewSearchRequest(
		ldapDefaultBaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dup*)(mail=*dup*))",
		[]string{"commonname", "mail"}, nil,
	)
	resAttrs, err := conn.Search(reqAttrs)
	if err != nil || len(resAttrs.Entries) != 3 {
		t.Fatalf("Search attrs: %v", err)
	}
	if len(resAttrs.Entries[0].Attributes) != 2 {
		t.Errorf("attributs filtrés = %d, attendu 2 (%v)", len(resAttrs.Entries[0].Attributes), resAttrs.Entries[0].Attributes)
	}
	if resAttrs.Entries[0].GetAttributeValue("cn") != "Jean Dupont" || resAttrs.Entries[0].GetAttributeValue("mail") != "jean.dupont@ex.fr" {
		t.Errorf("attributs filtrés inattendus: %+v", resAttrs.Entries[0].Attributes)
	}
}

func TestLDAPFilterExtraction(t *testing.T) {
	cases := []struct {
		filter string
		want   string
	}{
		{"(|(cn=*dup*)(mail=*dup*)(sn=*dup*)(givenName=*dup*))", "dup"},
		{"(&(objectClass=inetOrgPerson)(|(cn=*dup*)(mail=*dup*)))", "dup"},
		{"(&(|(cn=*jean*)(mail=*jean*))(|(cn=*dupont*)(mail=*dupont*)))", "jean dupont"},
		{"(|(cn=*françois*)(mail=*françois*))", "françois"},
		{"(mail=alice@ex.fr)", "alice@ex.fr"},
		{"(objectClass=*)", ""},
		{"(!(cn=*dup*))", ""},
	}
	for _, tc := range cases {
		pkt, err := ldap.CompileFilter(tc.filter)
		if err != nil {
			t.Fatalf("CompileFilter(%q): %v", tc.filter, err)
		}
		if got := extractQueryFromFilter(pkt); got != tc.want {
			t.Errorf("extractQueryFromFilter(%q) = %q, attendu %q", tc.filter, got, tc.want)
		}
	}
}

func TestLDAPMinQueryLength(t *testing.T) {
	fz := &fakeZimbra{}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)
	conn := dialTestLDAP(t, addr)
	if err := conn.Bind("alice@ex.fr", "secret"); err != nil {
		t.Fatal(err)
	}

	res, err := conn.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*d*)(mail=*d*))", nil, nil,
	))
	if err != nil {
		t.Fatalf("Search 1 char: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("got %d entries, want 0", len(res.Entries))
	}
	fz.mu.Lock()
	calls := fz.autoCompleteCalls
	fz.mu.Unlock()
	if calls != 0 {
		t.Errorf("autoCompleteCalls = %d, want 0", calls)
	}
}

func TestLDAPAuthFailuresAndBindDN(t *testing.T) {
	fz := &fakeZimbra{}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)

	// Recherche sans Bind préalable -> refusée (49).
	c0 := dialTestLDAP(t, addr)
	_, err := c0.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dup*))", nil, nil,
	))
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("search sans bind: %v, attendu code 49", err)
	}

	// Bind anonyme refusé.
	if err := c0.UnauthenticatedBind(""); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("bind anonyme: %v, attendu code 49", err)
	}

	// Mauvais mot de passe -> code 49.
	if err := c0.Bind("alice@ex.fr", "wrong"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("mauvais mot de passe: %v, attendu code 49", err)
	}

	// Bind DN complet (mail=alice@ex.fr,dc=zimbra,dc=local) -> accepté.
	c1 := dialTestLDAP(t, addr)
	if err := c1.Bind("mail=alice@ex.fr,dc=zimbra,dc=local", "secret"); err != nil {
		t.Fatalf("Bind DN complet: %v", err)
	}
	res, err := c1.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dup*))", nil, nil,
	))
	if err != nil || len(res.Entries) == 0 {
		t.Fatalf("Search après Bind DN: %v, len=%d", err, len(res.Entries))
	}

	// Zimbra injoignable -> code 52 (Unavailable) au Bind d'un compte non en cache
	// et lors d'une recherche non en cache.
	zs.Close()
	c2 := dialTestLDAP(t, addr)
	if err := c2.Bind("bob@ex.fr", "secret"); !ldap.IsErrorWithCode(err, ldap.LDAPResultUnavailable) {
		t.Errorf("Zimbra coupé (Bind): %v, attendu code 52", err)
	}
	if _, err := c1.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*nouveau*))", nil, nil,
	)); !ldap.IsErrorWithCode(err, ldap.LDAPResultUnavailable) {
		t.Errorf("Zimbra coupé (Search): %v, attendu code 52", err)
	}
}

func TestLDAPTokenCacheAndAutoRefresh(t *testing.T) {
	fz := &fakeZimbra{authLifetimeMs: 600_000}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)

	c1 := dialTestLDAP(t, addr)
	if err := c1.Bind("alice@ex.fr", "secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dup*))", nil, nil,
	)); err != nil {
		t.Fatal(err)
	}

	// 2e connexion avec mêmes identifiants : réutilise le token en cache.
	c2 := dialTestLDAP(t, addr)
	if err := c2.Bind("alice@ex.fr", "secret"); err != nil {
		t.Fatal(err)
	}
	fz.mu.Lock()
	auths := fz.authCalls
	fz.mu.Unlock()
	if auths != 1 {
		t.Fatalf("authCalls = %d, attendu 1 (cache token)", auths)
	}

	// Mauvais mot de passe sur 3e connexion : ne doit jamais passer malgré le cache.
	c3 := dialTestLDAP(t, addr)
	if err := c3.Bind("alice@ex.fr", "wrong"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Fatalf("mauvais mdp malgré cache: %v, attendu 49", err)
	}

	// Expiration simulée de tok-alice -> ré-authentification transparente.
	fz.mu.Lock()
	fz.expiredTokens = map[string]bool{"tok-alice": true}
	fz.tokenToIssue = "tok-fresh"
	fz.mu.Unlock()

	res, err := c2.Search(ldap.NewSearchRequest(
		ldapDefaultBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(cn=*dupo*))", nil, nil,
	))
	if err != nil || len(res.Entries) == 0 {
		t.Fatalf("Search après refresh token: %v, len=%d", err, len(res.Entries))
	}
	fz.mu.Lock()
	authsAfter := fz.authCalls
	fz.mu.Unlock()
	if authsAfter != 2 {
		t.Errorf("authCalls après refresh = %d, attendu 2", authsAfter)
	}
}

func TestLDAPAbandonKeepsConnectionAlive(t *testing.T) {
	waitCh := make(chan struct{})
	startedCh := make(chan struct{}, 1)
	canceledCh := make(chan struct{}, 1)
	fz := &fakeZimbra{
		autoCompleteWait:     waitCh,
		autoCompleteStarted:  startedCh,
		autoCompleteCanceled: canceledCh,
	}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)

	tcpConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpConn.Close()
	_ = tcpConn.SetDeadline(time.Now().Add(5 * time.Second))

	sendBER := func(pkt *ber.Packet) {
		t.Helper()
		if _, err := tcpConn.Write(pkt.Bytes()); err != nil {
			t.Fatalf("write BER: %v", err)
		}
	}
	readBER := func() *ber.Packet {
		t.Helper()
		p, err := ber.ReadPacket(tcpConn)
		if err != nil {
			t.Fatalf("read BER: %v", err)
		}
		return p
	}

	// 1. BindRequest (msgID = 1)
	bindReq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
	bindReq.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(1), "messageID"))
	bindOp := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationBindRequest, nil, "BindRequest")
	bindOp.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(3), "version"))
	bindOp.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "alice@ex.fr", "name"))
	bindOp.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 0, "secret", "simple"))
	bindReq.AppendChild(bindOp)
	sendBER(bindReq)

	bindResp := readBER()
	if code, _ := bindResp.Children[1].Children[0].Value.(int64); code != ldap.LDAPResultSuccess {
		t.Fatalf("BindResponse code = %d", code)
	}

	// 2. SearchRequest lent (msgID = 2)
	buildSearch := func(msgID int64, filterStr string) *ber.Packet {
		flt, err := ldap.CompileFilter(filterStr)
		if err != nil {
			t.Fatal(err)
		}
		msg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
		msg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "messageID"))
		op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchRequest, nil, "SearchRequest")
		op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, ldapDefaultBaseDN, "baseObject"))
		op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(ldap.ScopeWholeSubtree), "scope"))
		op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(ldap.NeverDerefAliases), "derefAliases"))
		op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(0), "sizeLimit"))
		op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(0), "timeLimit"))
		op.AppendChild(ber.NewBoolean(ber.ClassUniversal, ber.TypePrimitive, ber.TagBoolean, false, "typesOnly"))
		op.AppendChild(flt)
		op.AppendChild(ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes"))
		msg.AppendChild(op)
		return msg
	}

	sendBER(buildSearch(2, "(|(cn=*slow*))"))
	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("le faux Zimbra n'a pas reçu la requête SearchRequest")
	}

	// 3. AbandonRequest (msgID = 3, abandonne msgID = 2)
	abandonMsg := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
	abandonMsg.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(3), "messageID"))
	abandonOp := ber.NewInteger(ber.ClassApplication, ber.TypePrimitive, ldap.ApplicationAbandonRequest, int64(2), "AbandonRequest")
	abandonMsg.AppendChild(abandonOp)
	sendBER(abandonMsg)

	select {
	case <-canceledCh:
	case <-time.After(2 * time.Second):
		t.Fatal("AbandonRequest n'a pas annulé le contexte HTTP Zimbra")
	}

	// Débloquer les futurs appels et envoyer un SearchRequest (msgID = 4) sur la MÊME connexion.
	fz.mu.Lock()
	fz.autoCompleteWait = nil
	fz.mu.Unlock()
	sendBER(buildSearch(4, "(|(cn=*fast*))"))

	// Aucune réponse pour msgID=2 ne doit arriver, uniquement les entrées + done de msgID=4.
	gotDone := false
	gotEntries := 0
	for !gotDone {
		p := readBER()
		gotID, _ := p.Children[0].Value.(int64)
		if gotID != 4 {
			t.Fatalf("messageID inattendu %d (attendu 4)", gotID)
		}
		switch p.Children[1].Tag {
		case ldap.ApplicationSearchResultEntry:
			gotEntries++
		case ldap.ApplicationSearchResultDone:
			if code, _ := p.Children[1].Children[0].Value.(int64); code != ldap.LDAPResultSuccess {
				t.Fatalf("SearchResultDone code = %d", code)
			}
			gotDone = true
		default:
			t.Fatalf("tag inattendu: %d", p.Children[1].Tag)
		}
	}
	if gotEntries == 0 {
		t.Errorf("aucune entrée reçue après AbandonRequest")
	}
}

func TestLDAPStopWithOpenConnection(t *testing.T) {
	fz := &fakeZimbra{}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	srv, addr := startTestLDAP(t, zs.URL)
	conn := dialTestLDAP(t, addr)
	if err := conn.Bind("alice@ex.fr", "secret"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() bloqué alors qu'un client garde une connexion ouverte")
	}
	if got := srv.Status(); got != "" {
		t.Errorf("Status() après Stop = %q, attendu vide", got)
	}
}

func TestLDAPRootDSE(t *testing.T) {
	fz := &fakeZimbra{}
	zs := httptest.NewServer(fz)
	defer zs.Close()

	_, addr := startTestLDAP(t, zs.URL)
	conn := dialTestLDAP(t, addr)

	res, err := conn.Search(ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"supportedLDAPVersion", "namingContexts"}, nil,
	))
	if err != nil {
		t.Fatalf("Root DSE: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("Root DSE entries = %d, want 1", len(res.Entries))
	}
	if got := res.Entries[0].GetAttributeValue("supportedLDAPVersion"); got != "3" {
		t.Errorf("supportedLDAPVersion = %q, want 3", got)
	}
	if got := res.Entries[0].GetAttributeValue("namingContexts"); got != ldapDefaultBaseDN {
		t.Errorf("namingContexts = %q, want %q", got, ldapDefaultBaseDN)
	}
}
