package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	JobProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "blinky_jobs_processed_total",
			Help: "Total number of processed jobs by outcome and denoiser.",
		},
		[]string{"result", "denoiser"},
	)

	JobDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "blinky_job_duration_seconds",
			Help:    "Job processing duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"denoiser"},
	)

	LoudnessBefore = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "blinky_loudness_before_lufs",
			Help: "Loudness (LUFS) before processing.",
		},
		[]string{"denoiser"},
	)

	LoudnessAfter = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "blinky_loudness_after_lufs",
			Help: "Loudness (LUFS) after processing.",
		},
		[]string{"denoiser"},
	)

	SNRBefore = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "blinky_snr_before_db",
			Help: "Estimated SNR (dB) before processing.",
		},
		[]string{"denoiser"},
	)

	SNRAfter = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "blinky_snr_after_db",
			Help: "Estimated SNR (dB) after processing.",
		},
		[]string{"denoiser"},
	)

	SNRImprovement = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "blinky_snr_improvement_db",
			Help: "Estimated SNR improvement (after - before) in dB.",
		},
		[]string{"denoiser"},
	)
)

// Register registers metrics with Prometheus default registry.
func Register() {
	prometheus.MustRegister(JobProcessed)
	prometheus.MustRegister(JobDuration)
	prometheus.MustRegister(LoudnessBefore)
	prometheus.MustRegister(LoudnessAfter)
	prometheus.MustRegister(SNRBefore)
	prometheus.MustRegister(SNRAfter)
	prometheus.MustRegister(SNRImprovement)
}

// ObserveJob records job metrics
func ObserveJob(denoiser string, duration time.Duration, success bool,
	loudBefore, loudAfter float64,
	snrBefore, snrAfter float64) {
	res := "failed"
	if success {
		res = "success"
	}
	JobProcessed.WithLabelValues(res, denoiser).Inc()
	JobDuration.WithLabelValues(denoiser).Observe(duration.Seconds())
	LoudnessBefore.WithLabelValues(denoiser).Set(loudBefore)
	LoudnessAfter.WithLabelValues(denoiser).Set(loudAfter)
	SNRBefore.WithLabelValues(denoiser).Set(snrBefore)
	SNRAfter.WithLabelValues(denoiser).Set(snrAfter)
	SNRImprovement.WithLabelValues(denoiser).Set(snrAfter - snrBefore)
}
