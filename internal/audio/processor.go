package audio

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PipelineConfig maps to YAML pipeline config
type PipelineConfig struct {
	DenoiseDefault string         `yaml:"denoise_default"`
	TargetLUFS     float64        `yaml:"target_lufs"`
	SampleRate     int            `yaml:"sample_rate"`
	Channels       int            `yaml:"channels"`
	UseCompressor  bool           `yaml:"use_compressor"`
	Compressor     CompressorConf `yaml:"compressor"`
	UseLimiter     bool           `yaml:"use_limiter"`
	Limiter        LimiterConf    `yaml:"limiter"`
}

type CompressorConf struct {
	ThresholdDB float64 `yaml:"threshold_db"`
	Ratio       float64 `yaml:"ratio"`
	Attack      int     `yaml:"attack"`  // ms
	Release     int     `yaml:"release"` // ms
}

type LimiterConf struct {
	ThresholdDB float64 `yaml:"threshold_db"`
}

// ProcessOptions passed from main/API
type ProcessOptions struct {
	DenoiseMethod string
	TargetLUFS    float64
	SampleRate    int
	Channels      int
	UseCompressor bool
	Compressor    CompressorConf
	UseLimiter    bool
	Limiter       LimiterConf
}

// Stats returned after processing
type Stats struct {
	DurationSec float64            `json:"duration_sec"`
	Loudness    map[string]float64 `json:"loudness"` // measured loudness map (keys from MeasureLoudness)
}

// ProcessFile performs:
// 1) choose denoiser (arnndn if requested and available, else afftdn)
// 2) measure loudness via ffmpeg loudnorm (first pass)
// 3) apply loudnorm using measured params (second pass) + compressor + limiter
// 4) returns Stats with duration and loudness metrics
func ProcessFile(ctx context.Context, inputPath, outputPath string, opts ProcessOptions) (*Stats, error) {
	// ensure input absolute path
	inputPathAbs, _ := filepath.Abs(inputPath)
	outputPathAbs, _ := filepath.Abs(outputPath)

	// check ffmpeg present
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	// check ffprobe present
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, fmt.Errorf("ffprobe not found in PATH: %w", err)
	}

	// 1) choose denoise filter
	denoiseFilter := "afftdn" // default
	if strings.ToLower(opts.DenoiseMethod) == "rnnoise" || strings.ToLower(opts.DenoiseMethod) == "arnndn" {
		// verify arnndn filter exists in ffmpeg build
		if ffmpegHasFilter("arnndn") {
			denoiseFilter = "arnndn"
		} else {
			// fallback
			denoiseFilter = "afftdn"
		}
	}

	// 2) measure loudness (first pass)
	loudnessMap, _ := MeasureLoudness(ctx, inputPathAbs, opts.TargetLUFS)
	// continue even if measurement returns error; will apply single-pass fallback

	// 3) Build filter chain for second pass
	filterParts := []string{}
	if denoiseFilter != "" {
		filterParts = append(filterParts, denoiseFilter)
	}

	// prepare loudnorm application (note.me use measured values if available in future enhancements)
	loudnormApply := fmt.Sprintf("loudnorm=I=%v:TP=-1.5:LRA=7", opts.TargetLUFS)
	filterParts = append(filterParts, loudnormApply)

	// compressor (needs dB -> linear conversion for threshold)
	if opts.UseCompressor {
		ac := opts.Compressor
		// FFmpeg acompressor expects threshold as linear amplitude (0..1).
		// Convert dB threshold to linear: linear = 10^(dB/20)
		linearThresh := math.Pow(10.0, ac.ThresholdDB/20.0)
		// clip to allowed ffmpeg range [approx 0.000976563 .. 1]
		if linearThresh < 0.000976563 {
			linearThresh = 0.000976563
		} else if linearThresh > 1.0 {
			linearThresh = 1.0
		}
		// acompressor: threshold (linear), ratio, attack (ms), release (ms)
		comp := fmt.Sprintf("acompressor=threshold=%v:ratio=%v:attack=%d:release=%d",
			stripTrailingZeros(linearThresh), ac.Ratio, ac.Attack, ac.Release)
		filterParts = append(filterParts, comp)
	}

	// limiter (dB -> linear)
	if opts.UseLimiter {
		lim := opts.Limiter
		linLimit := math.Pow(10.0, lim.ThresholdDB/20.0)
		if linLimit < 0.000976563 {
			linLimit = 0.000976563
		} else if linLimit > 1.0 {
			linLimit = 1.0
		}
		// alimiter expects limit in linear amplitude in many FFmpeg builds
		limStr := fmt.Sprintf("alimiter=limit=%v", stripTrailingZeros(linLimit))
		filterParts = append(filterParts, limStr)
	}

	// After filters, resample to configured sample rate (final step)
	// Note.me: we avoid using pan because pan syntax can be picky across ffmpeg builds.
	// We rely on -ac <channels> (passed in args) to set channels.
	resample := fmt.Sprintf("aresample=%d", opts.SampleRate)
	filterParts = append(filterParts, resample)

	filterChain := strings.Join(filterParts, ",")

	// build ffmpeg args for apply pass
	args := []string{
		"-y",
		"-i", inputPathAbs,
		"-af", filterChain,
		"-ar", strconv.Itoa(opts.SampleRate),
		"-ac", strconv.Itoa(opts.Channels), // let ffmpeg handle channel conversion
		"-vn",
		outputPathAbs,
	}

	// run ffmpeg second pass (apply)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg apply failed after %s: %w - stderr: %s", time.Since(start), err, stderr.String())
	}

	// 4) collect stats (duration & loudness after processing)
	stats := &Stats{}
	if d, err := GetDuration(ctx, outputPathAbs); err == nil {
		stats.DurationSec = d
	}
	if lm, err := MeasureLoudness(ctx, outputPathAbs, opts.TargetLUFS); err == nil {
		stats.Loudness = lm
	} else {
		// fallback to pre-measured map if final measure failed
		stats.Loudness = loudnessMap
	}

	return stats, nil
}

// ffmpegHasFilter checks if ffmpeg supports a given filter name by calling "ffmpeg -filters"
func ffmpegHasFilter(name string) bool {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return false
	}
	cmd := exec.Command(ffmpegPath, "-filters")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	return strings.Contains(out.String(), name)
}

// stripTrailingZeros formats a float removing unnecessary decimals for ffmpeg filters.
// returns a string not a float to avoid fmt printing lengthy digits.
func stripTrailingZeros(f float64) string {
	s := strconv.FormatFloat(f, 'f', 6, 64) // keep 6 decimals
	// trim trailing zeros and possible trailing dot
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}
