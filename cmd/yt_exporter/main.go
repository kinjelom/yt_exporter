// Command yt_exporter exports JetBrains YouTrack metrics read from its REST API in the Prometheus format.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/kinjelom/yt_exporter/internal/collector"
	"github.com/kinjelom/yt_exporter/internal/config"
	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

const defaultListenAddress = ":9776"

func main() {
	var (
		configFile = kingpin.Flag("config.file",
			"Path to the configuration file (optional when YOUTRACK_URL and a token are set in the environment).").
			Envar("YT_EXPORTER_CONFIG_FILE").String()
		configCheck  = kingpin.Flag("config.check", "Validate the configuration and exit.").Bool()
		metricsPath  = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		toolkitFlags = kingpinflag.AddFlags(kingpin.CommandLine, defaultListenAddress)
		logConfig    = &promslog.Config{}
	)
	promslogflag.AddFlags(kingpin.CommandLine, logConfig)
	kingpin.Version(version.Print("yt_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promslog.New(logConfig)

	if err := run(logger, *configFile, *configCheck, *metricsPath, toolkitFlags); err != nil {
		logger.Error("yt_exporter failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, configFile string, configCheck bool, metricsPath string, toolkitFlags *web.FlagConfig) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	tlsConfig, err := cfg.YouTrack.TLSConfig()
	if err != nil {
		return err
	}
	if configCheck {
		logger.Info("configuration is valid")
		return nil
	}
	logger.Info("starting yt_exporter", "version", version.Info(), "build_context", version.BuildContext(),
		"youtrack", cfg.YouTrack.URL)

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		versioncollector.NewCollector("yt_exporter"),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	client, err := youtrack.New(youtrack.Options{
		URL:                   cfg.YouTrack.URL,
		Token:                 cfg.YouTrack.Token,
		Timeout:               cfg.YouTrack.Timeout,
		MaxConcurrentRequests: cfg.YouTrack.MaxConcurrentRequests,
		TLSConfig:             tlsConfig,
		Registerer:            reg,
	})
	if err != nil {
		return err
	}
	exporter := collector.New(client, cfg.Collectors, logger)
	reg.MustRegister(exporter)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exporter.Start(ctx)

	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Healthy\n"))
	})
	if metricsPath != "/" {
		landing, err := web.NewLandingPage(web.LandingConfig{
			Name:        "YouTrack Exporter",
			Description: "Prometheus exporter for JetBrains YouTrack",
			Version:     version.Info(),
			Links:       []web.LandingLinks{{Address: metricsPath, Text: "Metrics"}},
		})
		if err != nil {
			return err
		}
		mux.Handle("/", landing)
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := web.ListenAndServe(server, toolkitFlags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("yt_exporter stopped")
	return nil
}
