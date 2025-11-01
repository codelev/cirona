package cirona

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

var serviceStatus = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "cirona_service_status",
		Help: "Scheduled service status, 0 for down or 1 for up.",
	},
	[]string{"service"},
)

var jobExecutionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cirona_job_executions_total",
		Help: "Total number of job executions.",
	},
	[]string{"service", "schedule"},
)

var jobExecutionStatus = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "cirona_job_execution_status",
		Help: "Exit code, stdout and stderr of the job.",
	},
	[]string{"service", "replica", "exit_code", "stdout", "stderr"},
)

var jobExecutionDuration = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "cirona_job_execution_duration_ms",
		Help: "Duration of the job execution in milliseconds.",
	},
	[]string{"service", "replica"},
)

type metricEntry struct {
	labels    prometheus.Labels
	createdAt time.Time
}

var (
	executionMetrics []metricEntry
	executionMutex   sync.RWMutex
)

func MeterServiceStatus(service string, enabled bool) {
	if enabled {
		serviceStatus.WithLabelValues(service).Set(1)
	} else {
		serviceStatus.WithLabelValues(service).Set(0)
	}
}

func MeterJobExecutionsTotal(service, schedule string, total uint64) {
	jobExecutionsTotal.WithLabelValues(service, schedule).Add(float64(total))
}

func MeterJobExecution(service string, replica string, exitCode int, stdout string, stderr string, duration int64) {
	jobExecutionDuration.WithLabelValues(service, replica).Set(float64(duration))

	labels := prometheus.Labels{
		"service":   service,
		"replica":   replica,
		"exit_code": fmt.Sprintf("%d", exitCode),
		"stdout":    stdout,
		"stderr":    stderr,
	}

	jobExecutionStatus.With(labels).Set(float64(time.Now().Unix()))

	// Track for cleanup
	executionMutex.Lock()
	executionMetrics = append(executionMetrics, metricEntry{
		labels:    labels,
		createdAt: time.Now(),
	})
	executionMutex.Unlock()
}

func cleanupOldMetrics(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	executionMutex.Lock()
	defer executionMutex.Unlock()
	var keepMetrics []metricEntry
	deletedCount := 0
	for _, entry := range executionMetrics {
		if entry.createdAt.Before(cutoff) {
			jobExecutionStatus.Delete(entry.labels)
			deletedCount++
		} else {
			keepMetrics = append(keepMetrics, entry)
		}
	}
	if deletedCount > 0 {
		log.Debug().Int("deleted", deletedCount).Msg("Cleaned up old metrics")
	}
}

func InitMetrics() {
	prometheus.MustRegister(
		serviceStatus,
		jobExecutionsTotal,
		jobExecutionStatus,
		jobExecutionDuration,
	)

	log.Info().Msg("Creating metrics auto-cleanup ...")
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanupOldMetrics(60 * time.Minute)
		}
	}()
}
