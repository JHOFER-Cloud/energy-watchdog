// Command energy-watchdog powers a Proxmox host (p1) down when solar production no
// longer covers consumption and wakes it back up when surplus returns, shedding and
// restoring its guests around the cycle. See JHC-504.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/controller"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	if cfg.DryRun {
		log.Warn("running in dry-run mode: no actions will be taken")
	}

	store, err := newStore(cfg, log)
	if err != nil {
		log.Error("init state store", "err", err)
		os.Exit(1)
	}

	tlsConf, err := proxmox.TLSConfig(cfg.Proxmox.CACertPath, cfg.Proxmox.InsecureSkipVerify)
	if err != nil {
		log.Error("build proxmox TLS config", "err", err)
		os.Exit(1)
	}

	m := metrics.New(cfg.DryRun)
	m.SetThresholds(cfg.Prometheus.HeadroomWatts, cfg.Prometheus.ShedBelowWatts, cfg.Prometheus.MinBatteryPercent)
	ctrl := controller.New(
		cfg,
		prom.New(cfg.Prometheus.URL),
		proxmox.New(cfg.Proxmox.Endpoint, cfg.Proxmox.TokenID, cfg.Proxmox.TokenSecret, tlsConf),
		alertmgr.New(cfg.Alertmanager.URL),
		store,
		m,
		log,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go serveMetrics(ctx, cfg.MetricsAddr, m, log)

	log.Info("energy-watchdog started", "interval", cfg.Interval.Duration, "node", cfg.Proxmox.Node, "dryRun", cfg.DryRun)
	ctrl.Run(ctx)
	log.Info("energy-watchdog stopped")
}

func newStore(cfg *config.Config, log *slog.Logger) (state.Store, error) {
	if state.InCluster() {
		log.Info("using ConfigMap state store", "name", cfg.State.ConfigMapName)
		return state.NewConfigMapStore(cfg.State.ConfigMapName)
	}
	log.Info("using file state store", "path", cfg.State.FilePath)
	return state.NewFileStore(cfg.State.FilePath), nil
}

func serveMetrics(ctx context.Context, addr string, m *metrics.Metrics, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("metrics server", "err", err)
	}
}
