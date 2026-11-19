package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

const (
	ldapDefaultBaseDN    = "dc=zimbra,dc=local"
	ldapMinQueryLen      = 2
	ldapSearchTimeout    = 8 * time.Second
	ldapAuthTimeout      = 15 * time.Second
	ldapCacheTTL         = 5 * time.Minute
	ldapMaxCacheEntries  = 500
	ldapMaxTokenEntries  = 64
	ldapDefaultSizeLimit = 100
)

type tokenCacheKey struct {
	baseURL  string
	user     string
	passHash [32]byte
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

type searchCacheKey struct {
	baseURL    string
	user       string
	queryLower string
}

type cachedContacts struct {
	contacts  []AutoCompleteContact
	expiresAt time.Time
}

// LDAPServer est le serveur LDAP local dédié à l'autocomplétion Thunderbird via Zimbra.
type LDAPServer struct {
	zimbraURL atomic.Value // string

	mu       sync.Mutex
	ln       net.Listener
	addr     string
	stopping bool
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup

	cacheMu sync.Mutex
	tokens  map[tokenCacheKey]cachedToken
	cache   map[searchCacheKey]cachedContacts
}

// NewLDAPServer crée un serveur LDAP local pointant vers l'URL Zimbra donnée.
func NewLDAPServer(zimbraURL string) *LDAPServer {
	s := &LDAPServer{
		conns:  make(map[net.Conn]struct{}),
		tokens: make(map[tokenCacheKey]cachedToken),
		cache:  make(map[searchCacheKey]cachedContacts),
	}
	s.zimbraURL.Store(zimbraURL)
	return s
}

func (s *LDAPServer) ZimbraURL() string {
	return s.zimbraURL.Load().(string)
}

func (s *LDAPServer) SetZimbraURL(u string) {
	if old, _ := s.zimbraURL.Swap(u).(string); old != u {
		s.clearCaches()
	}
}

// Start écoute sur 127.0.0.1:port et renvoie immédiatement toute erreur d'écoute.
func (s *LDAPServer) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.stopping = false
	s.ln = l
	s.addr = addr
	s.wg.Add(1)
	go s.acceptLoop(l)
	log.Printf("annuaire LDAP actif sur %s -> %s", addr, s.ZimbraURL())
	return nil
}

// Stop ferme l'écoute et toutes les connexions LDAP actives puis attend leur arrêt.
func (s *LDAPServer) Stop() {
	s.mu.Lock()
	if s.ln == nil {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	ln := s.ln
	s.ln = nil
	s.addr = ""
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	_ = ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	s.clearCaches()
	log.Printf("annuaire LDAP arrêté")
}

// Status renvoie l'adresse d'écoute LDAP, vide si arrêté.
func (s *LDAPServer) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *LDAPServer) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("serveur LDAP: %v", err)
			}
			return
		}
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go s.handleConn(c)
	}
}

func (s *LDAPServer) clearCaches() {
	s.cacheMu.Lock()
	clear(s.tokens)
	clear(s.cache)
	s.cacheMu.Unlock()
}

func makeTokenKey(base, user, pass string) tokenCacheKey {
	return tokenCacheKey{
		baseURL:  base,
		user:     user,
		passHash: sha256.Sum256([]byte(pass)),
	}
}

func tokenCacheTTL(lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return 0
	}
	if lifetime <= 2*time.Minute {
		return lifetime / 2
	}
	return lifetime - time.Minute
}

func (s *LDAPServer) authenticate(ctx context.Context, user, pass string) (string, error) {
	base := s.ZimbraURL()
	key := makeTokenKey(base, user, pass)
	now := time.Now()

	s.cacheMu.Lock()
	if entry, ok := s.tokens[key]; ok {
		if now.Before(entry.expiresAt) {
			s.cacheMu.Unlock()
			return entry.token, nil
		}
		delete(s.tokens, key)
	}
	s.cacheMu.Unlock()

	return s.authenticateNoCache(ctx, base, user, pass)
}

func (s *LDAPServer) authenticateNoCache(ctx context.Context, base, user, pass string) (string, error) {
	authCtx, cancel := context.WithTimeout(ctx, ldapAuthTimeout)
	defer cancel()
	token, lifetime, err := zimbraAuthContext(authCtx, base, user, pass)
	if err != nil {
		return "", err
	}
	if ttl := tokenCacheTTL(lifetime); ttl > 0 {
		key := makeTokenKey(base, user, pass)
		now := time.Now()
		s.cacheMu.Lock()
		for k, v := range s.tokens {
			if !now.Before(v.expiresAt) {
				delete(s.tokens, k)
			}
		}
		if len(s.tokens) >= ldapMaxTokenEntries {
			clear(s.tokens)
		}
		s.tokens[key] = cachedToken{token: token, expiresAt: now.Add(ttl)}
		s.cacheMu.Unlock()
	}
	return token, nil
}

func (s *LDAPServer) evictToken(base, user, pass string) {
	key := makeTokenKey(base, user, pass)
	s.cacheMu.Lock()
	delete(s.tokens, key)
	s.cacheMu.Unlock()
}

func (s *LDAPServer) getCachedSearch(key searchCacheKey) ([]AutoCompleteContact, bool) {
	now := time.Now()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expiresAt) {
		delete(s.cache, key)
		return nil, false
	}
	return entry.contacts, true
}

func (s *LDAPServer) putCachedSearch(key searchCacheKey, contacts []AutoCompleteContact) {
	now := time.Now()
	copied := append([]AutoCompleteContact(nil), contacts...)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for k, v := range s.cache {
		if !now.Before(v.expiresAt) {
			delete(s.cache, k)
		}
	}
	if len(s.cache) >= ldapMaxCacheEntries {
		clear(s.cache)
	}
	s.cache[key] = cachedContacts{contacts: copied, expiresAt: now.Add(ldapCacheTTL)}
}

func (s *LDAPServer) searchContacts(ctx context.Context, user, pass, token, query string) ([]AutoCompleteContact, string, error) {
	base := s.ZimbraURL()
	sKey := searchCacheKey{baseURL: base, user: user, queryLower: strings.ToLower(query)}
	if cached, ok := s.getCachedSearch(sKey); ok {
		return cached, token, nil
	}

	callOnce := func(tok string) ([]AutoCompleteContact, error) {
		callCtx, cancel := context.WithTimeout(ctx, ldapSearchTimeout)
		defer cancel()
		return zimbraAutoComplete(callCtx, base, tok, query)
	}

	contacts, err := callOnce(token)
	if err != nil && isAuthExpired(err) && ctx.Err() == nil {
		s.evictToken(base, user, pass)
		newToken, authErr := s.authenticateNoCache(ctx, base, user, pass)
		if authErr != nil {
			return nil, token, authErr
		}
		token = newToken
		contacts, err = callOnce(token)
	}
	if err != nil {
		return nil, token, err
	}

	s.putCachedSearch(sKey, contacts)
	return contacts, token, nil
}

// ---------- Session & protocole LDAP ----------

type inflightSearch struct {
	seq    uint64
	cancel context.CancelFunc
}

type ldapSession struct {
	srv     *LDAPServer
	conn    net.Conn
	connCtx context.Context

	writeMu sync.Mutex

	authMu sync.RWMutex
	user   string
	pass   string
	token  string

	inflightMu  sync.Mutex
	inflightSeq uint64
	inflight    map[int64]inflightSearch
	reqWg       sync.WaitGroup
}

func (s *LDAPServer) handleConn(conn net.Conn) {
	connCtx, connCancel := context.WithCancel(context.Background())
	sess := &ldapSession{
		srv:      s,
		conn:     conn,
		connCtx:  connCtx,
		inflight: make(map[int64]inflightSearch),
	}
	defer func() {
		connCancel()
		sess.reqWg.Wait()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
		s.wg.Done()
	}()

	br := bufio.NewReader(conn)
	for {
		pkt, err := ber.ReadPacket(br)
		if err != nil {
			return
		}
		if !sess.dispatch(pkt) {
			return
		}
	}
}

func (sess *ldapSession) dispatch(pkt *ber.Packet) bool {
	if pkt == nil || pkt.ClassType != ber.ClassUniversal || pkt.TagType != ber.TypeConstructed || pkt.Tag != ber.TagSequence || len(pkt.Children) < 2 {
		return false
	}
	msgIDPkt := pkt.Children[0]
	if msgIDPkt.ClassType != ber.ClassUniversal || msgIDPkt.TagType != ber.TypePrimitive || msgIDPkt.Tag != ber.TagInteger {
		return false
	}
	msgID, ok := msgIDPkt.Value.(int64)
	if !ok {
		return false
	}
	op := pkt.Children[1]
	if op.ClassType != ber.ClassApplication {
		return false
	}

	switch op.Tag {
	case ldap.ApplicationBindRequest:
		sess.handleBind(msgID, op)
		return true

	case ldap.ApplicationSearchRequest:
		params, err := parseSearchParams(op)
		if err != nil {
			_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultProtocolError, err.Error())
			return true
		}
		reqCtx, reqCancel := context.WithCancel(sess.connCtx)
		sess.inflightMu.Lock()
		if prev, exists := sess.inflight[msgID]; exists {
			prev.cancel()
		}
		sess.inflightSeq++
		seq := sess.inflightSeq
		sess.inflight[msgID] = inflightSearch{seq: seq, cancel: reqCancel}
		sess.inflightMu.Unlock()

		sess.reqWg.Add(1)
		go func() {
			defer func() {
				sess.inflightMu.Lock()
				if cur, exists := sess.inflight[msgID]; exists && cur.seq == seq {
					delete(sess.inflight, msgID)
				}
				sess.inflightMu.Unlock()
				reqCancel()
				sess.reqWg.Done()
			}()
			sess.handleSearch(reqCtx, msgID, params)
		}()
		return true

	case ldap.ApplicationAbandonRequest:
		if targetID, err := ber.ParseInt64(op.Data.Bytes()); err == nil {
			sess.inflightMu.Lock()
			if entry, exists := sess.inflight[targetID]; exists {
				entry.cancel()
			}
			sess.inflightMu.Unlock()
		}
		return true

	case ldap.ApplicationUnbindRequest:
		return false

	case ldap.ApplicationExtendedRequest:
		_ = sess.writeResult(msgID, ldap.ApplicationExtendedResponse, ldap.LDAPResultUnwillingToPerform, "extended operation not supported")
		return true

	default:
		return false
	}
}

func (sess *ldapSession) setAuth(user, pass, token string) {
	sess.authMu.Lock()
	sess.user, sess.pass, sess.token = user, pass, token
	sess.authMu.Unlock()
}

func (sess *ldapSession) updateToken(user, pass, token string) {
	sess.authMu.Lock()
	if sess.user == user && sess.pass == pass {
		sess.token = token
	}
	sess.authMu.Unlock()
}

func (sess *ldapSession) getAuth() (user, pass, token string) {
	sess.authMu.RLock()
	user, pass, token = sess.user, sess.pass, sess.token
	sess.authMu.RUnlock()
	return user, pass, token
}

func extractBindUser(bindDN string) string {
	bindDN = strings.TrimSpace(bindDN)
	if bindDN == "" {
		return ""
	}
	if dn, err := ldap.ParseDN(bindDN); err == nil && len(dn.RDNs) > 0 && len(dn.RDNs[0].Attributes) > 0 {
		return strings.TrimSpace(dn.RDNs[0].Attributes[0].Value)
	}
	return bindDN
}

func (sess *ldapSession) handleBind(msgID int64, op *ber.Packet) {
	if op.TagType != ber.TypeConstructed || len(op.Children) < 3 {
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultProtocolError, "malformed bind request")
		return
	}
	verPkt := op.Children[0]
	if verPkt.ClassType != ber.ClassUniversal || verPkt.Tag != ber.TagInteger {
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultProtocolError, "invalid LDAP version")
		return
	}
	version, ok := verPkt.Value.(int64)
	if !ok || version != 3 {
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultProtocolError, "LDAPv3 required")
		return
	}
	authPkt := op.Children[2]
	if authPkt.ClassType != ber.ClassContext || authPkt.Tag != 0 {
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultAuthMethodNotSupported, "simple authentication required")
		return
	}

	user := extractBindUser(op.Children[1].Data.String())
	pass := authPkt.Data.String()
	if user == "" || pass == "" {
		sess.setAuth("", "", "")
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultInvalidCredentials, "credentials required")
		return
	}

	token, err := sess.srv.authenticate(sess.connCtx, user, pass)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		sess.setAuth("", "", "")
		var f *soapFault
		if errors.As(err, &f) {
			log.Printf("ldap auth %s refusée: %v", user, err)
			_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultInvalidCredentials, "invalid credentials")
			return
		}
		log.Printf("ldap auth %s: %v", user, err)
		_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultUnavailable, "Zimbra unreachable")
		return
	}

	sess.setAuth(user, pass, token)
	_ = sess.writeResult(msgID, ldap.ApplicationBindResponse, ldap.LDAPResultSuccess, "")
}

type ldapSearchParams struct {
	baseDN     string
	scope      int
	sizeLimit  int
	typesOnly  bool
	filter     *ber.Packet
	attributes []string
}

func parseSearchParams(op *ber.Packet) (ldapSearchParams, error) {
	if op.TagType != ber.TypeConstructed || len(op.Children) < 7 {
		return ldapSearchParams{}, errors.New("malformed search request")
	}
	scope, ok := op.Children[1].Value.(int64)
	if !ok || op.Children[1].Tag != ber.TagEnumerated {
		return ldapSearchParams{}, errors.New("invalid search scope")
	}
	sizeLimit, ok := op.Children[3].Value.(int64)
	if !ok || op.Children[3].Tag != ber.TagInteger {
		return ldapSearchParams{}, errors.New("invalid size limit")
	}
	typesOnly, ok := op.Children[5].Value.(bool)
	if !ok || op.Children[5].Tag != ber.TagBoolean {
		return ldapSearchParams{}, errors.New("invalid typesOnly")
	}

	var attrs []string
	if len(op.Children) >= 8 {
		attrSeq := op.Children[7]
		if attrSeq.ClassType != ber.ClassUniversal || attrSeq.TagType != ber.TypeConstructed || attrSeq.Tag != ber.TagSequence {
			return ldapSearchParams{}, errors.New("invalid attribute list")
		}
		attrs = make([]string, 0, len(attrSeq.Children))
		for _, a := range attrSeq.Children {
			attrs = append(attrs, a.Data.String())
		}
	}

	return ldapSearchParams{
		baseDN:     op.Children[0].Data.String(),
		scope:      int(scope),
		sizeLimit:  int(sizeLimit),
		typesOnly:  typesOnly,
		filter:     op.Children[6],
		attributes: attrs,
	}, nil
}

func (sess *ldapSession) handleSearch(ctx context.Context, msgID int64, params ldapSearchParams) {
	if strings.TrimSpace(params.baseDN) == "" && params.scope == ldap.ScopeBaseObject {
		attrs := filterEntryAttributes([]ldapAttr{
			{name: "objectClass", values: []string{"top"}},
			{name: "namingContexts", values: []string{ldapDefaultBaseDN}},
			{name: "supportedLDAPVersion", values: []string{"3"}},
		}, params.attributes, params.typesOnly)
		if err := sess.writeEntry(msgID, "", attrs); err != nil {
			return
		}
		_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, "")
		return
	}

	user, pass, token := sess.getAuth()
	if user == "" || token == "" {
		_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultInvalidCredentials, "bind required")
		return
	}

	query := extractQueryFromFilter(params.filter)
	if utf8.RuneCountInString(query) < ldapMinQueryLen {
		_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, "")
		return
	}

	contacts, newToken, err := sess.srv.searchContacts(ctx, user, pass, token, query)
	if newToken != token {
		sess.updateToken(user, pass, newToken)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		var f *soapFault
		if errors.As(err, &f) {
			log.Printf("ldap recherche %s: %v", user, err)
			code := ldap.LDAPResultOperationsError
			diag := "Zimbra API error"
			if f.code == "account.AUTH_FAILED" || isAuthExpired(err) {
				code = ldap.LDAPResultInvalidCredentials
				diag = "invalid credentials"
			}
			_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, code, diag)
			return
		}
		log.Printf("ldap recherche %s: %v", user, err)
		_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultUnavailable, "Zimbra unreachable")
		return
	}

	limit := ldapDefaultSizeLimit
	if params.sizeLimit > 0 && params.sizeLimit < limit {
		limit = params.sizeLimit
	}
	if len(contacts) > limit {
		contacts = contacts[:limit]
	}

	baseDN := strings.TrimSpace(params.baseDN)
	if baseDN == "" {
		baseDN = ldapDefaultBaseDN
	}

	for _, c := range contacts {
		if ctx.Err() != nil {
			return
		}
		dn := (&ldap.AttributeTypeAndValue{Type: "mail", Value: c.Email}).String() + "," + baseDN
		rawAttrs := []ldapAttr{
			{name: "objectClass", values: []string{"top", "person", "organizationalPerson", "inetOrgPerson"}},
			{name: "cn", values: []string{c.DisplayName}},
			{name: "displayName", values: []string{c.DisplayName}},
			{name: "mail", values: []string{c.Email}},
		}
		if c.LastName != "" {
			rawAttrs = append(rawAttrs, ldapAttr{name: "sn", values: []string{c.LastName}})
		}
		if c.FirstName != "" {
			rawAttrs = append(rawAttrs, ldapAttr{name: "givenName", values: []string{c.FirstName}})
		}
		attrs := filterEntryAttributes(rawAttrs, params.attributes, params.typesOnly)
		if err := sess.writeEntry(msgID, dn, attrs); err != nil {
			return
		}
	}

	if ctx.Err() != nil {
		return
	}
	_ = sess.writeResult(msgID, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, "")
}

type ldapAttr struct {
	name   string
	values []string
}

func canonicalAttrName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "commonname":
		return "cn"
	case "surname":
		return "sn"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func filterEntryAttributes(raw []ldapAttr, requested []string, typesOnly bool) []ldapAttr {
	allAttrs := len(requested) == 0
	var wanted map[string]bool
	if !allAttrs {
		wanted = make(map[string]bool, len(requested))
		for _, r := range requested {
			c := canonicalAttrName(r)
			if c == "*" || c == "" {
				allAttrs = true
				break
			}
			wanted[c] = true
		}
	}

	out := make([]ldapAttr, 0, len(raw))
	for _, a := range raw {
		if !allAttrs && !wanted[strings.ToLower(a.name)] {
			continue
		}
		var vals []string
		if !typesOnly {
			vals = a.values
		}
		out = append(out, ldapAttr{name: a.name, values: vals})
	}
	return out
}

var searchAttributes = map[string]bool{
	"cn":              true,
	"commonname":      true,
	"mail":            true,
	"sn":              true,
	"surname":         true,
	"givenname":       true,
	"displayname":     true,
	"mozillanickname": true,
	"uid":             true,
}

// extractQueryFromFilter extrait les termes saisis depuis l'arbre BER du filtre LDAP.
func extractQueryFromFilter(p *ber.Packet) string {
	if p == nil || p.ClassType != ber.ClassContext {
		return ""
	}
	switch p.Tag {
	case ldap.FilterOr:
		for _, child := range p.Children {
			if q := extractQueryFromFilter(child); q != "" {
				return q
			}
		}
		return ""

	case ldap.FilterAnd:
		var terms []string
		seen := make(map[string]struct{})
		for _, child := range p.Children {
			q := extractQueryFromFilter(child)
			if q == "" {
				continue
			}
			key := strings.ToLower(q)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			terms = append(terms, q)
		}
		return strings.Join(terms, " ")

	case ldap.FilterNot, ldap.FilterPresent:
		return ""

	case ldap.FilterEqualityMatch, ldap.FilterApproxMatch:
		if len(p.Children) < 2 {
			return ""
		}
		attr := strings.ToLower(strings.TrimSpace(p.Children[0].Data.String()))
		if !searchAttributes[attr] {
			return ""
		}
		return strings.TrimSpace(p.Children[1].Data.String())

	case ldap.FilterSubstrings:
		if len(p.Children) < 2 {
			return ""
		}
		attr := strings.ToLower(strings.TrimSpace(p.Children[0].Data.String()))
		if !searchAttributes[attr] {
			return ""
		}
		var parts []string
		for _, sub := range p.Children[1].Children {
			if sub.ClassType != ber.ClassContext {
				continue
			}
			if sub.Tag != ldap.FilterSubstringsInitial && sub.Tag != ldap.FilterSubstringsAny && sub.Tag != ldap.FilterSubstringsFinal {
				continue
			}
			if part := strings.TrimSpace(sub.Data.String()); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, " ")

	default:
		return ""
	}
}

func (sess *ldapSession) writePacket(pkt *ber.Packet) error {
	b := pkt.Bytes()
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	_, err := sess.conn.Write(b)
	return err
}

func (sess *ldapSession) writeResult(msgID int64, appTag ber.Tag, code int, diag string) error {
	pkt := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
	pkt.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "messageID"))
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, appTag, nil, "LDAPResult")
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(code), "resultCode"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diag, "diagnosticMessage"))
	pkt.AppendChild(op)
	return sess.writePacket(pkt)
}

func (sess *ldapSession) writeEntry(msgID int64, dn string, attrs []ldapAttr) error {
	pkt := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAPMessage")
	pkt.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "messageID"))
	entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchResultEntry, nil, "SearchResultEntry")
	entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, dn, "objectName"))
	attrList := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes")
	for _, a := range attrs {
		pa := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "PartialAttribute")
		pa.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, a.name, "type"))
		vals := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "vals")
		for _, v := range a.values {
			vals.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, "AttributeValue"))
		}
		pa.AppendChild(vals)
		attrList.AppendChild(pa)
	}
	entry.AppendChild(attrList)
	pkt.AppendChild(entry)
	return sess.writePacket(pkt)
}
