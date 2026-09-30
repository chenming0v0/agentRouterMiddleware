// Command agentrouter is a retrying reverse-proxy middleware in front of LLM
// API providers, with an embedded admin WebUI.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agentrouter/internal/api"
	"agentrouter/internal/config"
	"agentrouter/internal/proxy"
	"agentrouter/internal/store"
	"agentrouter/internal/webui"
)

// webDist embeds the compiled WebUI. The all: prefix is required so files
// beginning with '.' or '_' (such as the web/dist/.gitkeep placeholder) are
// included and go:embed never sees an empty directory.
//
//go:embed all:web/dist
var webDist embed.FS

// defaultLogDir holds persisted request-log metadata and body captures.
const defaultLogDir = "./data/logs"

// rootHandler dispatches between the admin API, static WebUI and the proxy.
// Business routes are pure pass-through: the only credential that matters there
// is the caller's own provider key, so an admin token value is never inspected
// or rejected on a proxy path.
type rootHandler struct {
	api    http.Handler
	static http.Handler
	proxy  http.Handler
}

func (h *rootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if api.IsAdminPath(p) {
		h.api.ServeHTTP(w, r)
		return
	}
	if isStaticRequest(r.Method, p) {
		h.static.ServeHTTP(w, r)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

// isStaticRequest limits static serving to UI entry points; anything else,
// including GET /v1/models, must reach the proxy.
func isStaticRequest(method, p string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	switch p {
	case "/", "/favicon.svg", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(p, "/assets/")
}

func main() {
	var (
		listenFlag     = flag.String("listen", config.DefaultListen, "listen address")
		configFlag     = flag.String("config", "./config.json", "path to the JSON config file")
		webDirFlag     = flag.String("web-dir", "", "serve WebUI from this directory instead of the embedded copy")
		adminTokenFlag = flag.String("admin-token", "", "admin bearer token (defaults to $AGENTROUTER_ADMIN_TOKEN)")
		logDirFlag     = flag.String("log-dir", defaultLogDir, "directory for persistent request-log and body files")
	)
	flag.Parse()

	explicitListen := flagWasSet("listen")
	explicitToken := flagWasSet("admin-token")
	token := adminToken(*adminTokenFlag, explicitToken)

	cfgStore, err := config.Load(*configFlag)
	if err != nil {
		log.Printf("config: %v; continuing with defaults", err)
	}
	cfg := cfgStore.Get()

	addr := listenAddress(explicitListen, *listenFlag, cfg.Listen)
	if err := checkAdminExposure(addr, token); err != nil {
		log.Fatalf("%v", err)
	}

	static, err := staticHandler(*webDirFlag)
	if err != nil {
		log.Fatalf("webui: %v", err)
	}

	logs, err := store.NewPersistentLogStore(cfg.LogLimit, *logDirFlag)
	if err != nil {
		log.Fatalf("log store: %v", err)
	}

	root := &rootHandler{
		api:    api.New(cfgStore, logs, token),
		static: static,
		proxy:  proxy.New(cfgStore, logs),
	}

	srv := &http.Server{Addr: addr, Handler: root, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("agentrouter: listening on %s, config %s, logs %s, %d upstreams", addr, *configFlag, *logDirFlag, len(cfg.Upstreams))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("agentrouter: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v; forcing close", err)
		_ = srv.Close()
	}
}

// checkAdminExposure refuses to bind a public (non-loopback) address unless an
// admin token is configured, so the admin API and proxy are never exposed with
// anonymous access by accident.
func checkAdminExposure(addr, token string) error {
	if token != "" || isLoopbackListen(addr) {
		return nil
	}
	return errors.New("refusing to listen on a public address without an admin token; " +
		"set AGENTROUTER_ADMIN_TOKEN or -admin-token, or bind 127.0.0.1")
}

// isLoopbackListen reports whether addr only accepts loopback connections. An
// empty host (":18851") binds every interface and is therefore not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// staticHandler returns the WebUI handler from disk when dir is set, otherwise
// from the embedded web/dist tree.
func staticHandler(dir string) (http.Handler, error) {
	if dir != "" {
		return webui.NewHandler(os.DirFS(dir)), nil
	}
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, err
	}
	return webui.NewHandler(sub), nil
}

// listenAddress gives an explicitly-set flag precedence, then the config file.
func listenAddress(explicit bool, flagValue, configValue string) string {
	if explicit {
		return flagValue
	}
	if configValue != "" {
		return configValue
	}
	return flagValue
}

// adminToken resolves the token from an explicit flag, then the environment.
// An explicitly empty flag disables token auth entirely.
func adminToken(flagValue string, explicit bool) string {
	if explicit {
		return flagValue
	}
	return os.Getenv("AGENTROUTER_ADMIN_TOKEN")
}

func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
