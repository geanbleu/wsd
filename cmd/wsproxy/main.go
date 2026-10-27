package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"wsd/internal/version"
)

// ── config ────────────────────────────────────────────────────────────────────

type Config struct {
	Domain     string     `json:"domain"`
	Listen     string     `json:"listen"`
	WsdSocket  string     `json:"wsd_socket"`
	HMACSecret string     `json:"hmac_secret"`
	CookieName string     `json:"cookie_name"`
	CookieTTL  string     `json:"cookie_ttl"`
	OIDC       OIDCConfig `json:"oidc"`
}

// OIDCConfig enables OIDC auth (optional). If Issuer is empty, token-only mode is used.
type OIDCConfig struct {
	Issuer       string `json:"issuer"` // e.g. https://accounts.google.com
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// redirect_uri is https://<id>.ws.<domain>/_oidc/callback by default.
	// Set ExternalScheme if wsproxy sits behind a TLS terminator (default "https").
	ExternalScheme string `json:"external_scheme"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config parse: %w", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.WsdSocket == "" {
		cfg.WsdSocket = "/run/wsd/wsd.sock"
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "wsd_auth"
	}
	if cfg.CookieTTL == "" {
		cfg.CookieTTL = "24h"
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("config: domain requis")
	}
	if cfg.HMACSecret == "" {
		return nil, fmt.Errorf("config: hmac_secret requis")
	}
	return &cfg, nil
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	configPath := flag.String("config", "/etc/wsd/wsproxy.json", "chemin vers wsproxy.json")
	showVersion := flag.Bool("version", false, "afficher la version et quitter")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("wsproxy"))
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ttl, err := time.ParseDuration(cfg.CookieTTL)
	if err != nil {
		log.Error("cookie_ttl invalide", "err", err)
		os.Exit(1)
	}

	h := &ProxyHandler{
		cfg:    cfg,
		log:    log,
		secret: []byte(cfg.HMACSecret),
		ttl:    ttl,
		actMap: make(map[string]time.Time),
		wsdClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", cfg.WsdSocket)
				},
			},
			Timeout: 35 * time.Second,
		},
	}

	if cfg.OIDC.Issuer != "" {
		if err := h.initOIDC(context.Background()); err != nil {
			log.Error("oidc init", "err", err)
			os.Exit(1)
		}
		log.Info("OIDC activé", "issuer", cfg.OIDC.Issuer)
	}

	log.Info("wsproxy démarré", "listen", cfg.Listen, "domain", cfg.Domain)
	if err := http.ListenAndServe(cfg.Listen, h); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// ── ProxyHandler ──────────────────────────────────────────────────────────────

type ProxyHandler struct {
	cfg          *Config
	log          *slog.Logger
	secret       []byte
	ttl          time.Duration
	wsdClient    *http.Client
	actMu        sync.Mutex
	actMap       map[string]time.Time // ws_id → last activity sent
	oidcProvider *gooidc.Provider
	oauth2Cfg    oauth2.Config
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsID, err := h.extractID(r.Host)
	if err != nil {
		http.Error(w, "workspace introuvable", http.StatusNotFound)
		return
	}
	log := h.log.With("ws_id", wsID)

	// OIDC callback: /_oidc/callback?code=...&state=...
	if r.URL.Path == "/_oidc/callback" && h.oidcProvider != nil {
		h.handleOIDCCallback(w, r, wsID)
		return
	}

	// Token exchange: ?tkn=<token>
	if tkn := r.URL.Query().Get("tkn"); tkn != "" {
		h.handleTokenExchange(w, r, wsID, tkn)
		return
	}

	// Auth cookie
	if !h.validCookie(r, wsID) {
		if h.oidcProvider != nil && acceptsHTML(r) {
			h.handleOIDCLogin(w, r, wsID)
			return
		}
		if acceptsHTML(r) {
			http.Error(w, "Non autorisé. Utilisez wsctl token <id> pour obtenir un lien d'accès.", http.StatusForbidden)
		} else {
			http.Error(w, "non autorisé", http.StatusForbidden)
		}
		return
	}

	// Ensure workspace is running, get upstream
	upstream, err := h.callEnsure(r.Context(), wsID)
	if err != nil {
		log.Warn("ensure", "err", err)
		if acceptsHTML(r) {
			h.sendWakePage(w)
		} else {
			http.Error(w, "workspace non disponible: "+err.Error(), http.StatusServiceUnavailable)
		}
		return
	}

	// Activity reporting (at most once per 30 s)
	h.maybeReportActivity(wsID)

	// Reverse proxy
	h.proxyRequest(w, r, upstream)
}

// ── token exchange ────────────────────────────────────────────────────────────

func (h *ProxyHandler) handleTokenExchange(w http.ResponseWriter, r *http.Request, wsID, tkn string) {
	resp, err := h.wsdClient.Post("http://wsd/v1/tokens/"+tkn, "application/json", nil)
	if err != nil {
		http.Error(w, "erreur validation token", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "token invalide", http.StatusForbidden)
		return
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["workspace_id"] != wsID {
		http.Error(w, "token invalide pour ce workspace", http.StatusForbidden)
		return
	}

	h.setCookie(w, r, wsID)

	// Redirect to same URL without tkn param
	q := r.URL.Query()
	q.Del("tkn")
	r.URL.RawQuery = q.Encode()
	http.Redirect(w, r, r.URL.String(), http.StatusFound)
}

// ── auth cookie ───────────────────────────────────────────────────────────────

func (h *ProxyHandler) setCookie(w http.ResponseWriter, r *http.Request, wsID string) {
	exp := time.Now().Add(h.ttl).Unix()
	sig := h.sign(wsID, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    fmt.Sprintf("%d.%s", exp, sig),
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *ProxyHandler) validCookie(r *http.Request, wsID string) bool {
	cookie, err := r.Cookie(h.cfg.CookieName)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return false
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return false
	}
	expected := h.sign(wsID, expUnix)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (h *ProxyHandler) sign(wsID string, expUnix int64) string {
	mac := hmac.New(sha256.New, h.secret)
	fmt.Fprintf(mac, "%s.%d", wsID, expUnix)
	return hex.EncodeToString(mac.Sum(nil))
}

// ── ensure ────────────────────────────────────────────────────────────────────

func (h *ProxyHandler) callEnsure(ctx context.Context, wsID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://wsd/v1/workspaces/"+wsID+"/ensure", nil)
	if err != nil {
		return "", err
	}
	resp, err := h.wsdClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ensure: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, e["error"])
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["upstream"] == "" {
		return "", fmt.Errorf("upstream absent dans la réponse ensure")
	}
	return result["upstream"], nil
}

// ── activity ──────────────────────────────────────────────────────────────────

func (h *ProxyHandler) maybeReportActivity(wsID string) {
	h.actMu.Lock()
	now := time.Now()
	last := h.actMap[wsID]
	if now.Sub(last) < 30*time.Second {
		h.actMu.Unlock()
		return
	}
	h.actMap[wsID] = now
	h.actMu.Unlock()
	go func() {
		resp, _ := h.wsdClient.Post(
			"http://wsd/v1/workspaces/"+wsID+"/activity", "application/json", nil)
		if resp != nil {
			resp.Body.Close()
		}
	}()
}

// ── reverse proxy ─────────────────────────────────────────────────────────────

func (h *ProxyHandler) proxyRequest(w http.ResponseWriter, r *http.Request, upstream string) {
	target := &url.URL{Scheme: "http", Host: upstream}
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			if req.Header.Get("X-Forwarded-For") == "" {
				req.Header.Set("X-Forwarded-For", r.RemoteAddr)
			}
			req.Header.Set("X-Forwarded-Host", r.Host)
		},
		FlushInterval: -1, // streaming et WebSocket
	}
	rp.ServeHTTP(w, r)
}

// ── wake page ─────────────────────────────────────────────────────────────────

var wakeHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="2">
<title>Workspace en démarrage…</title>
<style>body{font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#1e1e2e;color:#cdd6f4}div{text-align:center}</style>
</head><body><div>
<h2>Workspace en cours de démarrage…</h2>
<p>Cette page se recharge automatiquement.</p>
</div></body></html>`

func (h *ProxyHandler) sendWakePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(wakeHTML))
}

// ── OIDC ──────────────────────────────────────────────────────────────────────

func (h *ProxyHandler) initOIDC(ctx context.Context) error {
	provider, err := gooidc.NewProvider(ctx, h.cfg.OIDC.Issuer)
	if err != nil {
		return fmt.Errorf("oidc provider: %w", err)
	}
	h.oidcProvider = provider
	h.oauth2Cfg = oauth2.Config{
		ClientID:     h.cfg.OIDC.ClientID,
		ClientSecret: h.cfg.OIDC.ClientSecret,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{gooidc.ScopeOpenID, "email"},
	}
	return nil
}

func (h *ProxyHandler) oidcRedirectURI(wsID string) string {
	scheme := h.cfg.OIDC.ExternalScheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s.ws.%s/_oidc/callback", scheme, wsID, h.cfg.Domain)
}

// oidcState generates a short-lived HMAC-signed state token for CSRF protection.
func (h *ProxyHandler) oidcState(wsID string) string {
	b := make([]byte, 8)
	rand.Read(b)
	nonce := hex.EncodeToString(b)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, h.secret)
	fmt.Fprintf(mac, "oidcstate:%s:%s:%s", wsID, ts, nonce)
	sig := hex.EncodeToString(mac.Sum(nil))[:16]
	return wsID + ":" + ts + ":" + nonce + ":" + sig
}

// parseOIDCState verifies the state and returns the workspace ID.
func (h *ProxyHandler) parseOIDCState(state string) (wsID string, ok bool) {
	parts := strings.SplitN(state, ":", 4)
	if len(parts) != 4 {
		return "", false
	}
	wsID, ts, nonce, sig := parts[0], parts[1], parts[2], parts[3]
	tsUnix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Now().Unix()-tsUnix > 600 {
		return "", false
	}
	mac := hmac.New(sha256.New, h.secret)
	fmt.Fprintf(mac, "oidcstate:%s:%s:%s", wsID, ts, nonce)
	expected := hex.EncodeToString(mac.Sum(nil))[:16]
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return "", false
	}
	return wsID, true
}

func (h *ProxyHandler) handleOIDCLogin(w http.ResponseWriter, r *http.Request, wsID string) {
	cfg := h.oauth2Cfg
	cfg.RedirectURL = h.oidcRedirectURI(wsID)
	state := h.oidcState(wsID)
	http.Redirect(w, r, cfg.AuthCodeURL(state), http.StatusFound)
}

func (h *ProxyHandler) handleOIDCCallback(w http.ResponseWriter, r *http.Request, wsID string) {
	// 1. Vérifier le state CSRF
	wsFromState, ok := h.parseOIDCState(r.URL.Query().Get("state"))
	if !ok || wsFromState != wsID {
		http.Error(w, "état OIDC invalide", http.StatusBadRequest)
		return
	}

	// 2. Échanger le code contre un token
	cfg := h.oauth2Cfg
	cfg.RedirectURL = h.oidcRedirectURI(wsID)
	oauthToken, err := cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "échange OIDC échoué", http.StatusInternalServerError)
		h.log.Warn("oidc exchange", "err", err)
		return
	}

	// 3. Vérifier et décoder l'ID token
	verifier := h.oidcProvider.Verifier(&gooidc.Config{ClientID: h.cfg.OIDC.ClientID})
	rawIDToken, ok := oauthToken.Extra("id_token").(string)
	if !ok {
		http.Error(w, "id_token absent", http.StatusInternalServerError)
		return
	}
	idToken, err := verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "id_token invalide", http.StatusForbidden)
		return
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims OIDC invalides", http.StatusInternalServerError)
		return
	}
	userID := claims.Email
	if userID == "" {
		userID = claims.Sub
	}

	// 4. Vérifier que l'owner du workspace correspond
	ws, err := h.getWorkspace(r.Context(), wsID)
	if err != nil {
		http.Error(w, "workspace introuvable", http.StatusNotFound)
		return
	}
	if owner, _ := ws["owner"].(string); owner != "" && owner != userID {
		h.log.Warn("oidc accès refusé", "ws_id", wsID, "owner", owner, "user", userID)
		http.Error(w, "accès refusé : ce workspace appartient à "+owner, http.StatusForbidden)
		return
	}

	// 5. Émettre le cookie HMAC et rediriger
	h.setCookie(w, r, wsID)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *ProxyHandler) getWorkspace(ctx context.Context, wsID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://wsd/v1/workspaces/"+wsID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.wsdClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (h *ProxyHandler) extractID(host string) (string, error) {
	hostOnly, _, _ := strings.Cut(host, ":")
	suffix := ".ws." + h.cfg.Domain
	if !strings.HasSuffix(hostOnly, suffix) {
		return "", fmt.Errorf("host %q sans préfixe workspace", host)
	}
	id := strings.TrimSuffix(hostOnly, suffix)
	if id == "" {
		return "", fmt.Errorf("id vide dans %q", host)
	}
	return id, nil
}

func acceptsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if mt == "text/html" || mt == "*/*" {
			return true
		}
	}
	return false
}
