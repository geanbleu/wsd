package main

import (
	"context"
	"crypto/hmac"
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
)

// ── config ────────────────────────────────────────────────────────────────────

type Config struct {
	Domain     string `json:"domain"`
	Listen     string `json:"listen"`
	WsdSocket  string `json:"wsd_socket"`
	HMACSecret string `json:"hmac_secret"`
	CookieName string `json:"cookie_name"`
	CookieTTL  string `json:"cookie_ttl"`
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
	flag.Parse()

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

	log.Info("wsproxy démarré", "listen", cfg.Listen, "domain", cfg.Domain)
	if err := http.ListenAndServe(cfg.Listen, h); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// ── ProxyHandler ──────────────────────────────────────────────────────────────

type ProxyHandler struct {
	cfg       *Config
	log       *slog.Logger
	secret    []byte
	ttl       time.Duration
	wsdClient *http.Client
	actMu     sync.Mutex
	actMap    map[string]time.Time // ws_id → last activity sent
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsID, err := h.extractID(r.Host)
	if err != nil {
		http.Error(w, "workspace introuvable", http.StatusNotFound)
		return
	}
	log := h.log.With("ws_id", wsID)

	// Token exchange: ?tkn=<token>
	if tkn := r.URL.Query().Get("tkn"); tkn != "" {
		h.handleTokenExchange(w, r, wsID, tkn)
		return
	}

	// Auth cookie
	if !h.validCookie(r, wsID) {
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
