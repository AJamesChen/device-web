package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "device_web_session"
	sessionTTL    = 12 * time.Hour
)

//go:embed static/*
var assets embed.FS

type Config struct {
	Username      string
	Password      string
	OTAAPIURL     string
	OTAAPIToken   string
	SecureCookies bool
	MaxUpload     int64
}

type session struct {
	csrf    string
	expires time.Time
}

type Server struct {
	config   Config
	apiURL   *url.URL
	client   *http.Client
	sessions map[string]session
	mu       sync.Mutex
	static   http.Handler
}

func New(config Config) (*Server, error) {
	if config.Username == "" || config.Password == "" {
		return nil, errors.New("username and password must not be empty")
	}
	if config.OTAAPIToken == "" {
		return nil, errors.New("OTA API token must not be empty")
	}
	if config.MaxUpload <= 0 {
		return nil, errors.New("maximum upload size must be positive")
	}
	apiURL, err := url.Parse(config.OTAAPIURL)
	if err != nil || (apiURL.Scheme != "http" && apiURL.Scheme != "https") || apiURL.Host == "" {
		return nil, fmt.Errorf("invalid OTA API URL %q", config.OTAAPIURL)
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	return &Server{
		config:   config,
		apiURL:   apiURL,
		client:   &http.Client{Timeout: 30 * time.Minute},
		sessions: make(map[string]session),
		static:   http.FileServer(http.FS(staticFS)),
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	switch {
	case r.URL.Path == "/login" && r.Method == http.MethodGet:
		s.serveAsset(w, r, "login.html")
	case r.URL.Path == "/login" && r.Method == http.MethodPost:
		s.login(w, r)
	case r.URL.Path == "/health" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case strings.HasPrefix(r.URL.Path, "/static/") && r.Method == http.MethodGet:
		http.StripPrefix("/static/", s.static).ServeHTTP(w, r)
	default:
		s.authenticated(w, r)
	}
}

func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) {
	sessionID, current, ok := s.currentSession(r)
	if !ok {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		s.serveAsset(w, r, "index.html")
	case r.URL.Path == "/api/session" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"username": s.config.Username, "csrf_token": current.csrf})
	case r.URL.Path == "/api/system" && r.Method == http.MethodGet:
		s.proxyJSON(w, r, http.MethodGet, "/api/v1/system", nil)
	case r.URL.Path == "/api/update" && r.Method == http.MethodGet:
		s.proxyJSON(w, r, http.MethodGet, "/api/v1/update", nil)
	case r.URL.Path == "/api/firmware" && r.Method == http.MethodPost:
		if !validCSRF(r, current) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
			return
		}
		s.uploadFirmware(w, r)
	case r.URL.Path == "/logout" && r.Method == http.MethodPost:
		if !validCSRF(r, current) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		s.deleteSession(sessionID)
		s.clearCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.loginFailure(w, r)
		return
	}
	usernameOK := subtle.ConstantTimeCompare([]byte(r.FormValue("username")), []byte(s.config.Username)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.config.Password)) == 1
	if !usernameOK || !passwordOK {
		time.Sleep(250 * time.Millisecond)
		s.loginFailure(w, r)
		return
	}
	sessionID, err := randomToken(32)
	if err != nil {
		http.Error(w, "cannot create session", http.StatusInternalServerError)
		return
	}
	csrf, err := randomToken(24)
	if err != nil {
		http.Error(w, "cannot create session", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.removeExpiredLocked(time.Now())
	s.sessions[sessionID] = session{csrf: csrf, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sessionID, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) loginFailure(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
}

func (s *Server) currentSession(r *http.Request) (string, session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", session{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[cookie.Value]
	if !ok || now.After(current.expires) {
		delete(s.sessions, cookie.Value)
		return "", session{}, false
	}
	return cookie.Value, current, true
}

func (s *Server) deleteSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *Server) removeExpiredLocked(now time.Time) {
	for id, current := range s.sessions {
		if now.After(current.expires) {
			delete(s.sessions, id)
		}
	}
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func validCSRF(r *http.Request, current session) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(current.csrf)) == 1
}

func (s *Server) proxyJSON(w http.ResponseWriter, r *http.Request, method, path string, body io.Reader) {
	target := s.apiURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(r.Context(), method, target.String(), body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot build OTA API request"})
		return
	}
	request.Header.Set("Authorization", "Bearer "+s.config.OTAAPIToken)
	response, err := s.client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "OTA API is unavailable"})
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 1<<20))
}

func (s *Server) uploadFirmware(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength < 0 {
		writeJSON(w, http.StatusLengthRequired, map[string]string{"error": "Content-Length is required"})
		return
	}
	if r.ContentLength == 0 || r.ContentLength > s.config.MaxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "firmware bundle is empty or too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxUpload)
	target := s.apiURL.ResolveReference(&url.URL{Path: "/api/v1/update"})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot build update request"})
		return
	}
	request.ContentLength = r.ContentLength
	request.Header.Set("Authorization", "Bearer "+s.config.OTAAPIToken)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := s.client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "firmware upload failed"})
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 1<<20))
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	content, err := assets.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
}

func randomToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
