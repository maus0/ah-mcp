package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/mrserzhan/ah-mcp/tools"
)

// version is set at build time via -ldflags="-X main.version=v1.2.3"
var version = "dev"

// appieVersion returns the version of github.com/gwillem/appie-go from build info.
func appieVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/gwillem/appie-go" {
			return dep.Version
		}
	}
	return "unknown"
}

const (
	// 127.0.0.1, not localhost: hCaptcha refuses to run on a page served from
	// "localhost", and AH's login page will not submit without it.
	defaultCallbackHost = "http://127.0.0.1:9876"
	defaultCallbackPort = 9876
	defaultMCPPort      = 3000

	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second

	// healthPath answers without auth so container runtimes and reverse
	// proxies can probe liveness. It reveals nothing but "ok".
	healthPath         = "/healthz"
	healthcheckTimeout = 3 * time.Second
)

func main() {
	transport := flag.String("transport", "sse", "Transport mode: 'sse', 'streamable-http', or 'stdio'")
	remote := flag.Bool("remote", false, "Remote mode: bind all interfaces and disable auto browser-open on login (overridden by AH_REMOTE=true)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "Probe the running HTTP server's "+healthPath+" and exit 0 if healthy (for container healthchecks)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("ah-mcp %s (appie-go %s)\n", version, appieVersion())
		os.Exit(0)
	}

	if *healthcheck {
		if err := probeHealth(healthcheckURL(envIntOr("AH_MCP_PORT", defaultMCPPort))); err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Init structured logger (always stderr + optional file via AH_LOG_FILE).
	tools.InitLogger(os.Getenv("AH_LOG_FILE"))

	// AH_REMOTE env var also enables remote mode.
	if os.Getenv("AH_REMOTE") == "true" {
		*remote = true
	}

	// Resolve config from environment.
	callbackHost := envOr("AH_CALLBACK_HOST", defaultCallbackHost)
	callbackPort := envIntOr("AH_CALLBACK_PORT", defaultCallbackPort)
	mcpPort := envIntOr("AH_MCP_PORT", defaultMCPPort)
	tokensPath := TokensPath()
	mcpToken := os.Getenv("AH_MCP_TOKEN")
	oauthCfg, err := oauthConfigFromEnv()
	if err != nil {
		tools.LogError("startup", "%v", err)
		os.Exit(1)
	}

	tools.LogInfo("startup", "config version=%s site=%s transport=%s remote=%t callback_host=%s callback_port=%d mcp_port=%d auth=%t oauth=%t log_file_set=%t",
		version, ahSite(), *transport, *remote, callbackHost, callbackPort, mcpPort, mcpToken != "", oauthCfg != nil, os.Getenv("AH_LOG_FILE") != "")
	if u, err := url.Parse(callbackHost); err == nil && strings.EqualFold(u.Hostname(), "localhost") {
		tools.LogWarn("startup", "AH_CALLBACK_HOST uses localhost; hCaptcha refuses that hostname, so the AH login will fail with a captcha error. Use http://127.0.0.1:%d instead", callbackPort)
	}

	// Ensure token directory exists with secure permissions.
	if err := os.MkdirAll(filepath.Dir(tokensPath), 0700); err != nil {
		tools.LogWarn("startup", "could not create token directory: %v", err)
	}

	// Build MCP server.
	s := server.NewMCPServer(
		"Albert Heijn",
		version,
		server.WithLogging(),
	)

	// Build dependency bundle.
	deps := tools.Deps{
		TokensPath:    tokensPath,
		RemoteMode:    *remote,
		ServerVersion: version,
		AppieVersion:  appieVersion(),
		GetClient:     GetClient,
		ReloadClient:  ReloadClient,
		IsAuthenticated: func() bool {
			return IsAuthenticated(tokensPath)
		},
		StartOAuthFlow: func() (*tools.OAuthFlow, error) {
			return StartOAuthFlow(callbackHost, callbackPort, tokensPath, *remote)
		},
		RefreshIfNeeded: func(ctx context.Context) error {
			refreshed, err := RefreshIfNeeded(ctx, tokensPath)
			if err != nil {
				return err
			}
			// The cached appie client holds the tokens it was created with.
			// After a refresh those are stale — and because AH rotates refresh
			// tokens, reusing them would eventually fail every call until the
			// process restarts. Rebuild the client from the file we just wrote.
			if refreshed {
				if _, err := ReloadClient(); err != nil {
					return fmt.Errorf("reload client after token refresh: %w", err)
				}
			}
			return nil
		},
	}

	// Register all tools.
	tools.RegisterLoginTool(s, deps)
	tools.RegisterProductTools(s, deps)
	tools.RegisterOrderTools(s, deps)
	tools.RegisterBasketTools(s, deps)
	tools.RegisterMemberTools(s, deps)
	tools.RegisterDeliveryTools(s, deps)
	tools.RegisterInfoTool(s, deps)

	ctx := context.Background()
	appieVer := appieVersion()
	baseURL := envOr("AH_MCP_BASE_URL", fmt.Sprintf("http://localhost:%d", mcpPort))
	auth := newAuthenticator(mcpToken, oauthCfg, baseURL)
	if oauthCfg != nil && os.Getenv("AH_MCP_BASE_URL") == "" && *transport != "stdio" {
		tools.LogWarn("startup", "OAuth is enabled but AH_MCP_BASE_URL is unset; clients will be told this server lives at %s", baseURL)
	}

	switch *transport {
	case "stdio":
		tools.LogInfo("startup", "starting stdio transport version=%s appie_go=%s", version, appieVer)
		stdioSrv := server.NewStdioServer(s)
		if err := stdioSrv.Listen(ctx, os.Stdin, os.Stdout); err != nil {
			tools.LogError("startup", "stdio server error: %v", err)
			os.Exit(1)
		}
	case "sse", "":
		addr, err := bindAddr(mcpPort, *remote)
		if err != nil {
			tools.LogError("startup", "%v", err)
			os.Exit(1)
		}
		if err := checkTransportAuth(addr, mcpToken, oauthCfg != nil); err != nil {
			tools.LogError("startup", "%v", err)
			os.Exit(1)
		}
		tools.LogInfo("startup", "starting SSE transport addr=%s version=%s appie_go=%s base_url=%s auth=%t oauth=%t",
			addr, version, appieVer, baseURL, mcpToken != "", oauthCfg != nil)
		sseSrv := server.NewSSEServer(s, server.WithBaseURL(baseURL), server.WithKeepAlive(true), server.WithKeepAliveInterval(5*time.Second))
		serve(addr, wrapHandler(sseSrv, auth, baseURL, mcpPort, true))
	case "streamable-http":
		addr, err := bindAddr(mcpPort, *remote)
		if err != nil {
			tools.LogError("startup", "%v", err)
			os.Exit(1)
		}
		if err := checkTransportAuth(addr, mcpToken, oauthCfg != nil); err != nil {
			tools.LogError("startup", "%v", err)
			os.Exit(1)
		}
		tools.LogInfo("startup", "starting Streamable HTTP transport addr=%s version=%s appie_go=%s base_url=%s auth=%t oauth=%t",
			addr, version, appieVer, baseURL, mcpToken != "", oauthCfg != nil)
		httpSrv := server.NewStreamableHTTPServer(s,
			server.WithEndpointPath("/mcp"),
			server.WithHeartbeatInterval(5*time.Second),
		)
		serve(addr, wrapHandler(httpSrv, auth, baseURL, mcpPort, false))
	default:
		tools.LogError("startup", "unknown transport %q — use 'sse', 'streamable-http', or 'stdio'", *transport)
		os.Exit(1)
	}
}

// bindAddr resolves the listen address. Local runs stay on the loopback
// interface: the server speaks for a logged-in Albert Heijn account, so
// listening on 0.0.0.0 would hand the whole account (cart, orders, receipts,
// address, date of birth) to anyone on the same network. Remote deployments
// opt in explicitly via --remote/AH_REMOTE, or AH_MCP_BIND for containers.
func bindAddr(port int, remote bool) (string, error) {
	if host := os.Getenv("AH_MCP_BIND"); host != "" {
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			return "", fmt.Errorf("AH_MCP_BIND must be a host without a port, got %q", host)
		}
		return fmt.Sprintf("%s:%d", host, port), nil
	}
	if remote {
		return fmt.Sprintf("0.0.0.0:%d", port), nil
	}
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

// isLoopbackBind reports whether addr only accepts connections from this host.
func isLoopbackBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// checkTransportAuth refuses to expose an authenticated AH account to the
// network without a token or OAuth, and warns when a loopback server has neither.
func checkTransportAuth(addr, token string, oauth bool) error {
	if token != "" || oauth {
		return nil
	}
	if !isLoopbackBind(addr) {
		return fmt.Errorf("refusing to listen on %s without AH_MCP_TOKEN or AH_MCP_OAUTH_ISSUER: this server acts on your Albert Heijn account "+
			"and would be usable by anyone who can reach that address. Configure auth, or drop --remote/AH_MCP_BIND to bind loopback only", addr)
	}
	tools.LogWarn("startup", "neither AH_MCP_TOKEN nor OAuth is configured — any process on this machine can use your Albert Heijn session")
	return nil
}

// serve runs an HTTP server with sane timeouts and graceful shutdown.
func serve(addr string, handler http.Handler) {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No ReadTimeout/WriteTimeout: SSE responses are long-lived by design.
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		tools.LogInfo("shutdown", "signal received, draining connections")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
	}()

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		tools.LogError("startup", "server error: %v", err)
		os.Exit(1)
	}
	tools.LogInfo("shutdown", "stopped")
}

// wrapHandler applies Origin validation and, when configured, auth. With
// OAuth enabled it also serves the protected resource metadata, which must be
// reachable without credentials: it is how a client learns where to get them.
func wrapHandler(next http.Handler, auth *authenticator, baseURL string, port int, sse bool) http.Handler {
	if auth != nil {
		if sse {
			next = tokenAuthMiddleware(auth, next)
		} else {
			next = simpleAuthMiddleware(auth, next)
		}
	}
	next = originMiddleware(allowedOrigins(baseURL, port), next)
	if auth != nil && auth.jwt != nil {
		metadata := protectedResourceHandler(baseURL, auth.jwt.issuer)
		inner := next
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProtectedResourcePath(r.URL.Path) {
				metadata.ServeHTTP(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	return healthMiddleware(next)
}

// healthMiddleware serves healthPath ahead of Origin and token checks, so a
// probe needs no credentials.
func healthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// healthcheckURL points at the local server's health endpoint. A wildcard
// bind is probed over loopback, since that is where the probe runs.
func healthcheckURL(port int) string {
	host := strings.Trim(os.Getenv("AH_MCP_BIND"), "[]")
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + healthPath
}

// probeHealth reports whether url answers 200.
func probeHealth(url string) error {
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}

// envOr returns the value of the named environment variable or the default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envIntOr returns the integer value of the named environment variable or the default.
func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// tokenEqual compares two tokens without leaking their contents through
// response timing.
func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// allowedOrigins builds the browser origins permitted to talk to this server.
// Override the whole set with AH_MCP_ALLOWED_ORIGINS (comma separated);
// "*" disables the check entirely.
func allowedOrigins(baseURL string, port int) []string {
	if v := os.Getenv("AH_MCP_ALLOWED_ORIGINS"); v != "" {
		var out []string
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				out = append(out, strings.ToLower(strings.TrimSuffix(o, "/")))
			}
		}
		return out
	}
	out := []string{
		fmt.Sprintf("http://localhost:%d", port),
		fmt.Sprintf("http://127.0.0.1:%d", port),
		"https://claude.ai",
		"https://claude.com",
	}
	if u, err := url.Parse(baseURL); err == nil && u.Scheme != "" && u.Host != "" {
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out
}

// originMiddleware blocks cross-origin browser requests, which is what stops a
// malicious web page from driving a loopback MCP server via DNS rebinding.
// Requests without an Origin header (every non-browser MCP client) pass through.
func originMiddleware(allowed []string, next http.Handler) http.Handler {
	allowAll := false
	for _, a := range allowed {
		if a == "*" {
			allowAll = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || allowAll {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.ToLower(strings.TrimSuffix(origin, "/"))
		for _, a := range allowed {
			if got == a {
				next.ServeHTTP(w, r)
				return
			}
		}
		tools.LogWarn("http", "rejected request with disallowed Origin %q (set AH_MCP_ALLOWED_ORIGINS to permit it)", origin)
		http.Error(w, "Forbidden origin", http.StatusForbidden)
	})
}

var (
	errNoCredentials = errors.New("no credentials")
	errInvalidToken  = errors.New("invalid token")
)

// authenticator accepts the static AH_MCP_TOKEN and, when OAuth is
// configured, JWT access tokens from the authorization server.
type authenticator struct {
	token               string       // static AH_MCP_TOKEN; "" when unset
	jwt                 *jwtVerifier // nil when OAuth is not configured
	resourceMetadataURL string
}

// newAuthenticator returns nil when neither a static token nor OAuth is set.
func newAuthenticator(token string, oauth *oauthConfig, baseURL string) *authenticator {
	if token == "" && oauth == nil {
		return nil
	}
	a := &authenticator{token: token}
	if oauth != nil {
		a.jwt = newJWTVerifier(oauth.Issuer, oauth.Audience, nil)
		a.resourceMetadataURL = strings.TrimSuffix(baseURL, "/") + protectedResourcePath
	}
	return a
}

// check accepts:
//   - Authorization: Bearer <AH_MCP_TOKEN or JWT>
//   - ?token=<AH_MCP_TOKEN>  (static token only: the MCP spec forbids access
//     tokens in the query string)
func (a *authenticator) check(r *http.Request) error {
	if h := r.Header.Get("Authorization"); h != "" {
		if a.token != "" && tokenEqual(h, "Bearer "+a.token) {
			return nil
		}
		scheme, raw, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || a.jwt == nil {
			return errInvalidToken
		}
		if _, err := a.jwt.Verify(r.Context(), strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("%w: %v", errInvalidToken, err)
		}
		return nil
	}
	if q := r.URL.Query().Get("token"); q != "" {
		if a.token != "" && tokenEqual(q, a.token) {
			return nil
		}
		return errInvalidToken
	}
	return errNoCredentials
}

// reject writes a 401. With OAuth enabled it carries the RFC 9728 challenge
// that points the client at the protected resource metadata.
func (a *authenticator) reject(w http.ResponseWriter, err error) {
	if a.resourceMetadataURL != "" {
		challenge := fmt.Sprintf(`Bearer resource_metadata="%s"`, a.resourceMetadataURL)
		if errors.Is(err, errInvalidToken) {
			challenge = fmt.Sprintf(`Bearer error="invalid_token", resource_metadata="%s"`, a.resourceMetadataURL)
			tools.LogWarn("http", "rejected bearer token: %v", err)
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// tokenAuthMiddleware rejects requests that the authenticator does not accept.
//
// Once an SSE connection is authenticated, the sessionId it receives is
// whitelisted so that subsequent /message posts (which don't carry the token)
// are also allowed. The entry is dropped when the SSE stream ends, so a
// session id cannot be replayed after the client disconnects.
func tokenAuthMiddleware(auth *authenticator, next http.Handler) http.Handler {
	var sessions sync.Map // sessionId string -> struct{}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /message: allow if sessionId was established by an authenticated SSE connection.
		if strings.HasPrefix(r.URL.Path, "/message") {
			if sid := r.URL.Query().Get("sessionId"); sid != "" {
				if _, ok := sessions.Load(sid); ok {
					next.ServeHTTP(w, r)
					return
				}
			}
		}

		if err := auth.check(r); err != nil {
			auth.reject(w, err)
			return
		}

		// For SSE connections, wrap the ResponseWriter to capture the sessionId
		// from the "event: endpoint" SSE message and whitelist it for as long
		// as the stream is open.
		if strings.HasPrefix(r.URL.Path, "/sse") {
			sc := &sessionCapture{ResponseWriter: w, sessions: &sessions}
			defer sc.forget()
			next.ServeHTTP(sc, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// sessionCapture wraps ResponseWriter to intercept SSE writes and extract
// the sessionId advertised in the "event: endpoint" message.
type sessionCapture struct {
	http.ResponseWriter
	sessions *sync.Map
	mu       sync.Mutex
	sid      string
}

func (sc *sessionCapture) Write(b []byte) (int, error) {
	if idx := bytes.Index(b, []byte("sessionId=")); idx >= 0 {
		rest := string(b[idx+len("sessionId="):])
		if end := strings.IndexAny(rest, "& \n\r"); end != -1 {
			rest = rest[:end]
		}
		if rest != "" {
			sc.mu.Lock()
			sc.sid = rest
			sc.mu.Unlock()
			sc.sessions.Store(rest, struct{}{})
		}
	}
	return sc.ResponseWriter.Write(b)
}

// forget revokes the captured sessionId once the SSE stream is finished.
func (sc *sessionCapture) forget() {
	sc.mu.Lock()
	sid := sc.sid
	sc.mu.Unlock()
	if sid != "" {
		sc.sessions.Delete(sid)
	}
}

func (sc *sessionCapture) Flush() {
	if f, ok := sc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// simpleAuthMiddleware authenticates every request.
// Used for streamable-http where each request is independent (no session tracking needed).
func simpleAuthMiddleware(auth *authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := auth.check(r); err != nil {
			auth.reject(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}
