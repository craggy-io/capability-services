package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
)

// DefaultAddr is the listen address when Config.Addr is empty.
const DefaultAddr = "127.0.0.1:8787"

// MCPPath is where the MCP projection is mounted when Config.MCP is set.
const MCPPath = "/mcp"

// Config is everything the server needs. The host owns every policy in it.
type Config struct {
	// Addr is the TCP listen address. Empty means DefaultAddr.
	Addr string
	// Name and Version identify the server in OpenAPI and MCP.
	Name, Version string
	// API is the view served over HTTP at /v1. Required.
	API *registry.View
	// MCP is the view served over MCP at /mcp. Nil serves no MCP.
	MCP *registry.View
	// MCPCaller is the caller every MCP call runs as. Kind defaults to "agent".
	MCPCaller registry.Caller
	// Authenticate is the host's authentication policy. It runs before every
	// request, including /mcp and Routes. It returns the request's caller, or an
	// error, which is answered 401 with a denied envelope. Nil means requests are
	// anonymous, which New allows only on a loopback Addr.
	Authenticate func(*http.Request) (registry.Caller, error)
	// Routes are host handlers mounted beside the API, such as "/v1/events".
	// They are authenticated like everything else.
	Routes map[string]http.Handler
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server serves one registry view over HTTP and, optionally, MCP.
type Server struct {
	cfg     Config
	log     *slog.Logger
	handler http.Handler
	stopMCP func()
}

// New assembles the handler. It refuses to serve a non-loopback address without
// an Authenticate policy.
func New(cfg Config) (*Server, error) {
	if cfg.API == nil {
		return nil, errors.New("server: Config.API is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Authenticate == nil {
		loopback, err := isLoopback(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("server: parse listen address %q: %w", cfg.Addr, err)
		}
		if !loopback {
			return nil, fmt.Errorf("server: %q is not a loopback address and no Authenticate policy is set", cfg.Addr)
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, log: log, stopMCP: func() {}}
	mux := http.NewServeMux()
	for path, h := range cfg.Routes {
		mux.Handle(path, h)
	}
	if cfg.MCP != nil {
		h, stop := s.mcpHandler()
		mux.Handle(MCPPath, h)
		s.stopMCP = stop
	}
	mux.Handle("/", httpapi.Handler(cfg.API, httpapi.Info(cfg.Name, cfg.Version)))
	s.handler = s.authenticate(mux)
	return s, nil
}

// Handler is the assembled, authenticated handler, for tests and for hosts that
// run their own http.Server.
func (s *Server) Handler() http.Handler { return s.handler }

// Close releases the MCP projection. Serve calls it on return.
func (s *Server) Close() { s.stopMCP() }

func (s *Server) mcpHandler() (http.Handler, func()) {
	sdk := mcpsdk.NewServer(&mcpsdk.Implementation{Name: s.cfg.Name, Version: s.cfg.Version}, nil)
	caller := s.cfg.MCPCaller
	stop := mcp.Serve(sdk, s.cfg.MCP, mcp.Caller(func(*mcpsdk.ServerSession) registry.Caller { return caller }))
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sdk },
		&mcpsdk.StreamableHTTPOptions{Logger: s.log, Stateless: true})
	return http.NewCrossOriginProtection().Handler(h), stop
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.cfg.Authenticate == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := s.cfg.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "denied", err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(registry.WithCaller(r.Context(), caller)))
	})
}

// Listener is a bound port that answers 503 until a Server is attached with
// Serve. Binding early lets clients see "starting" instead of "refused" while
// the host opens its services.
type Listener struct {
	log     *slog.Logger
	ln      net.Listener
	srv     *http.Server
	handler atomic.Pointer[http.Handler]
	served  chan error
}

// Listen binds addr and starts answering 503 with a retryable unavailable
// envelope.
func Listen(addr string, log *slog.Logger) (*Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	if addr == "" {
		addr = DefaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen %s: %w", addr, err)
	}
	l := &Listener{log: log, ln: ln, served: make(chan error, 1)}
	var starting http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the server is starting; retry shortly")
	})
	l.handler.Store(&starting)
	l.srv = &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*l.handler.Load()).ServeHTTP(w, r) }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { l.served <- l.srv.Serve(ln) }()
	return l, nil
}

// Addr is the bound address, useful with port 0.
func (l *Listener) Addr() string { return l.ln.Addr().String() }

// Close stops the listener without serving.
func (l *Listener) Close() { _ = l.srv.Close() }

// Serve switches the listener to s and blocks until ctx ends, then shuts down
// gracefully (5s) and closes s.
func (l *Listener) Serve(ctx context.Context, s *Server) error {
	defer s.Close()
	h := s.handler
	l.handler.Store(&h)
	l.log.Info("server listening", "addr", l.Addr(), "mcp", s.cfg.MCP != nil, "auth", s.cfg.Authenticate != nil)
	stopped := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.srv.Shutdown(shutdown)
	})
	defer stopped()
	if err := <-l.served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: serve: %w", err)
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "retryable": code == "unavailable"},
	})
}

func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}
