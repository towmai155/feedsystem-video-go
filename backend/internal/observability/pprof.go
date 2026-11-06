package observability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

type PprofServer struct {
	name            string
	server          *http.Server
	shutdownTimeout time.Duration
}

func NewPprofMux(databases ...*sql.DB) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	if len(databases) > 0 && databases[0] != nil {
		pool := databases[0]
		mux.HandleFunc("GET /debug/dbstats", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pool.Stats())
		})
	}

	return mux
}

func NewPprofServer(name string, enabled bool, addr string, databases ...*sql.DB) (*PprofServer, error) {
	if !enabled || addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to start %s pprof server on %s: %w", name, addr, err)
	}
	pprofServer := &PprofServer{
		name:            name,
		shutdownTimeout: 3 * time.Second,
	}
	pprofServer.server = &http.Server{
		Addr:              addr,
		Handler:           NewPprofMux(databases...),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("%s pprof listening on %s", name, addr)
		if err := pprofServer.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("%s pprof server error: %v", name, err)
		}
	}()
	return pprofServer, nil
}

func Shutdown(ctx context.Context, srv *http.Server) error {
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *PprofServer) Close() error {
	if s == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := Shutdown(shutdownCtx, s.server); err != nil {
		log.Printf("Failed to shutdown %s pprof server: %v", s.name, err)
		return err
	}
	return nil
}
