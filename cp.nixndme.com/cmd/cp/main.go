// Command cp is a single-user, self-hosted drop box for text, files and links that expire.
//
//	cp                  run the server
//	cp healthcheck      exit 0 if the local server answers /healthz (for Docker HEALTHCHECK)
//	cp reset-password   set a new password (CP_NEW_PASSWORD or a generated one) and turn off 2FA
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"cpshare/internal/auth"
	"cpshare/internal/config"
	"cpshare/internal/ids"
	"cpshare/internal/server"
	"cpshare/internal/store"
	"cpshare/internal/vault"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "":
		err = serve(cfg, log)
	case "healthcheck":
		err = healthcheck(cfg)
	case "reset-password":
		err = resetPassword(cfg)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cp:", err)
		os.Exit(1)
	}
}

func serve(cfg config.Config, log *slog.Logger) error {
	st, v, err := openData(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := ensureUser(cfg, st); err != nil {
		return err
	}
	kick := make(chan struct{}, 1)
	srv, err := server.New(cfg, st, v, log, kick)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go srv.RunCleanup(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute, // one chunk at a time, but slow mobile links need room
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
		// No WriteTimeout: a 100 MB download over a slow link must not be cut off.
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("cp listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
	return nil
}

func openData(cfg config.Config) (*store.Store, *vault.Vault, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	v, err := vault.Open(filepath.Join(cfg.DataDir, "blobs"), cfg.KeyFile)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, v, nil
}

// ensureUser creates the login account on first boot from CP_ADMIN_USER / CP_ADMIN_PASSWORD.
// Afterwards those variables are ignored; the password is changed from the settings page.
func ensureUser(cfg config.Config, st *store.Store) error {
	n, err := st.CountUsers()
	if err != nil || n > 0 {
		return err
	}
	user, pass := cfg.AdminUser, cfg.AdminPassword
	if user == "" {
		user = "admin"
	}
	generated := pass == ""
	if generated {
		pass = ids.New(20)
	}
	if len(pass) < config.MinPasswordLength() {
		return fmt.Errorf("CP_ADMIN_PASSWORD must be at least %d characters", config.MinPasswordLength())
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}
	if err := st.CreateUser(user, hash); err != nil {
		return err
	}
	if generated {
		fmt.Fprintf(os.Stderr, "first boot: created user %q with generated password %s (change it in Settings)\n", user, pass)
	}
	return nil
}

func resetPassword(cfg config.Config) error {
	st, _, err := openData(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	pass := os.Getenv("CP_NEW_PASSWORD")
	generated := pass == ""
	if generated {
		pass = ids.New(20)
	}
	if len(pass) < config.MinPasswordLength() {
		return fmt.Errorf("CP_NEW_PASSWORD must be at least %d characters", config.MinPasswordLength())
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}
	if err := st.ResetCredentials(hash); err != nil {
		return err
	}
	if generated {
		fmt.Printf("new password: %s\n", pass)
	}
	fmt.Println("password reset, 2FA turned off, every session logged out")
	return nil
}

func healthcheck(cfg config.Config) error {
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
