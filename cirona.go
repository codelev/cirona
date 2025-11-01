package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/codelev/cirona/internal"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	scheduler  *cirona.Scheduler
	version    = "dev"
	healthPort = ":9100"
	logLevel   = getEnv("LOGGING", "info")
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	var err error

	// Init
	configureLogger()
	log.Info().Msgf("Starting Cirona %s", version)
	cirona.InitMetrics()
	go func() { configureHealthcheck() }()

	// Handle os signals
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-channel
		if scheduler != nil {
			scheduler.CloseScheduler()
		}
		log.Info().Msgf("Caught signal %v", sig)
		os.Exit(1)
	}()

	// Init
	scheduler, err = cirona.NewScheduler()
	if err != nil {
		log.Fatal().Err(err).Msg("Cannot initialize Cirona")
	}

	// Run
	if err := scheduler.RunScheduler(); err != nil {
		log.Panic().Err(err).Msg("")
	}
}

func configureLogger() {
	var err error
	var w = zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC1123,
	}
	log.Logger = zerolog.New(w).With().Timestamp().Logger()
	logLevel, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		log.Fatal().Err(err).Msgf("Unknown log level")
	} else {
		zerolog.SetGlobalLevel(logLevel)
	}
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func configureHealthcheck() {
	server := &http.Server{
		Addr: healthPort,
		Handler: func() http.Handler {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("up"))
			})
			mux.Handle("/metrics", promhttp.Handler())
			return mux
		}(),
	}
	ln, err := net.Listen("tcp", healthPort)
	if err != nil {
		log.Fatal().Err(err).Msgf("Healthcheck listening error: %v", err)
	}
	log.Info().Msgf("Listening healthcheck on %s", healthPort)
	if err := server.Serve(ln); err != nil {
		log.Fatal().Err(err).Msgf("Healthcheck error: %v", err)
	}
}
