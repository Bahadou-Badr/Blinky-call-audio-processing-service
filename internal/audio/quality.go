package audio

import (
	"context"
	"fmt"
	"log"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// QualityMetrics holds signal quality data extracted from FFmpeg or analysis.
type QualityMetrics struct {
	SNR        float64   `json:"snr"`         // Signal-to-noise ratio (dB)
	RMSLevel   float64   `json:"rms_level"`   // Root mean square loudness (dB)
	PeakLevel  float64   `json:"peak_level"`  // Peak loudness (dB)
	NoiseLevel float64   `json:"noise_level"` // Estimated background noise (dB)
	Duration   float64   `json:"duration"`    // Duration of the audio (seconds)
	AnalyzedAt time.Time `json:"analyzed_at"` // Timestamp of when it was analyzed
}

// EstimateQuality runs an FFmpeg filter to estimate SNR and other stats.
func EstimateQuality(ctx context.Context, path string) (*QualityMetrics, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-nostats",
		"-i", path,
		"-filter_complex", "astats=metadata=1:reset=1",
		"-f", "null", "-")

	output, err := cmd.CombinedOutput()
	if err != nil {
		//FFmpeg may exit nonzero even with useful output
		log.Printf("[quality] warning: ffmpeg astats error: %v", err)
	}

	lines := strings.Split(string(output), "\n")

	var rmsVals, peakVals, noiseVals []float64

	reRMS := regexp.MustCompile(`RMS_level_dB:\s*(-?\d+\.\d+)`)
	rePeak := regexp.MustCompile(`Peak_level_dB:\s*(-?\d+\.\d+)`)
	reNoise := regexp.MustCompile(`Noise_level:\s*(-?\d+\.\d+)`)

	for _, line := range lines {
		if m := reRMS.FindStringSubmatch(line); len(m) == 2 {
			if v, _ := strconv.ParseFloat(m[1], 64); v != 0 {
				rmsVals = append(rmsVals, v)
			}
		}
		if m := rePeak.FindStringSubmatch(line); len(m) == 2 {
			if v, _ := strconv.ParseFloat(m[1], 64); v != 0 {
				peakVals = append(peakVals, v)
			}
		}
		if m := reNoise.FindStringSubmatch(line); len(m) == 2 {
			if v, _ := strconv.ParseFloat(m[1], 64); v != 0 {
				noiseVals = append(noiseVals, v)
			}
		}
	}

	if len(rmsVals) == 0 {
		return nil, fmt.Errorf("no RMS_level values found")
	}

	// compute averages
	rmsAvg := avg(rmsVals)
	peakAvg := avg(peakVals)
	noiseAvg := avg(noiseVals)

	// approximate SNR = peak - RMS, fallback to 0 if missing
	snr := 0.0
	if !math.IsNaN(peakAvg) && !math.IsNaN(rmsAvg) {
		snr = peakAvg - rmsAvg
	}

	return &QualityMetrics{
		SNR:        snr,
		RMSLevel:   rmsAvg,
		PeakLevel:  peakAvg,
		NoiseLevel: noiseAvg,
		Duration:   0.0, // can be filled later if needed
		AnalyzedAt: start,
	}, nil
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
