// Package server assembles the HTTP, MCP and Server-Sent Events projections of
// a registry view into one authenticated http.Handler, and serves it on a port
// that binds early and answers 503 until the host is ready.
//
// It is assembly, not policy. The host supplies every policy decision through
// Config: the views to serve, the authentication function that turns a request
// into a registry.Caller, the caller MCP calls run as, and any extra routes such
// as a change feed served with Events. The one rule the server keeps on its own
// is that it refuses to serve a non-loopback address without an Authenticate
// policy.
//
// Typical host use:
//
//	l, _ := server.Listen(addr, log)           // answers 503 while starting
//	// ... open services, build the registry ...
//	s, _ := server.New(server.Config{API: api, MCP: agents, Authenticate: auth,
//		Routes: map[string]http.Handler{"/v1/events": server.Events(hub)}})
//	_ = l.Serve(ctx, s)                         // blocks until ctx ends
//
// Exported names: Config, Server, New, Listener, Listen, Events, DefaultAddr,
// MCPPath.
package server
