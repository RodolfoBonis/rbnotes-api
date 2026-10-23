// Package httpserver encapsula o servidor HTTP com shutdown gracioso.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
)

// Server envolve o *http.Server com o ciclo de vida (Run + graceful shutdown).
type Server struct {
	srv             *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
}

// New constrói o servidor HTTP a partir da config, do handler e do logger.
func New(cfg config.HTTP, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		srv: &http.Server{
			Addr:    cfg.Addr,
			Handler: handler,
			// ReadHeaderTimeout (não ReadTimeout) protege contra slowloris sem cortar uploads grandes
			// de áudio (chunks de até 50 MB). O corpo é limitado por http.MaxBytesReader nos handlers.
			ReadHeaderTimeout: cfg.ReadTimeout,
			// WriteTimeout vale como deadline por resposta; SSE e download estendem via ResponseController.
			WriteTimeout: cfg.WriteTimeout,
		},
		logger:          logger,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// NewAddr constrói um servidor simples num endereço arbitrário (ex.: /metrics do worker).
func NewAddr(addr string, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		srv:             &http.Server{Addr: addr, Handler: handler, ReadTimeout: 10 * time.Second},
		logger:          logger,
		shutdownTimeout: 5 * time.Second,
	}
}

// Run sobe o servidor e bloqueia até ctx ser cancelado, então faz shutdown gracioso dentro de
// ShutdownTimeout. Devolve nil no encerramento limpo.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server iniciado", slog.String("addr", s.srv.Addr))
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()
		s.logger.Info("http server encerrando", slog.String("addr", s.srv.Addr))
		return s.srv.Shutdown(shutdownCtx)
	}
}
