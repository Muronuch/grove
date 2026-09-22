package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Muronuch/grove/internal/meta"
)

type Options struct {
	ProxyAddr string
	AdminAddr string
	Token     string
	StatePath string
	Log       *slog.Logger
}

var TokenEnv = meta.EnvVarName("ROUTER", "TOKEN")

var StatePathEnv = meta.EnvVarName("ROUTER", "STATE")

const DefaultStatePath = "/data/routes.json"

func Serve(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ProxyAddr == "" {
		o.ProxyAddr = fmt.Sprintf(":%d", meta.RouterProxyPort)
	}
	if o.AdminAddr == "" {
		o.AdminAddr = fmt.Sprintf(":%d", meta.RouterAdminPort)
	}
	if o.Token == "" {
		o.Token = os.Getenv(TokenEnv)
	}
	if o.Token == "" {
		return fmt.Errorf("the router needs its admin token in %s", TokenEnv)
	}
	if o.StatePath == "" {
		o.StatePath = os.Getenv(StatePathEnv)
	}
	if o.StatePath == "" {
		o.StatePath = DefaultStatePath
	}

	store := NewStore()
	activity := NewActivity()
	proxy := NewProxy(store, activity, o.Log)
	admin := NewAdmin(store, activity, proxy, o.Token, o.StatePath, o.Log)

	if err := admin.Restore(); err != nil {
		o.Log.Warn("could not restore the route table", "err", err)
	}

	proxySrv := &http.Server{
		Addr:              o.ProxyAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 15 * time.Second,

		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	adminSrv := &http.Server{
		Addr:              o.AdminAddr,
		Handler:           admin.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errs := make(chan error, 2)
	go func() {
		o.Log.Info("proxy listening", "addr", o.ProxyAddr)
		errs <- ignoreClosed(proxySrv.ListenAndServe())
	}()
	go func() {
		o.Log.Info("admin listening", "addr", o.AdminAddr)
		errs <- ignoreClosed(adminSrv.ListenAndServe())
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(shutdown)
	_ = adminSrv.Shutdown(shutdown)
	return nil
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
