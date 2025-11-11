package audio

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	NoiseLevel  float64            `json:"noise_level"`
}

// ProcessFile performs:
// 1) choose denoiser (arnndn if requested and available, else afftdn) or noisereduce
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

	// Mesure the noise level
	noiseLevel, err := GetNoiseLevel(ctx, inputPathAbs)
	if err != nil {
		return nil, fmt.Errorf("GetNoiseLevel  not work with the PATH: %w", err)
	}

	// 1) choose denoise filter (FFmpeg side only)
	denoiseFilter := "" // empty means "no ffmpeg-side denoising filter"
	dnMethod := strings.ToLower(strings.TrimSpace(opts.DenoiseMethod))

	// using the external python noisereduce helper, use its output as the new input.
	if dnMethod == "noisereduce" {
		denoisedPath, err := runNoisereduce(ctx, inputPathAbs, 1.0, "")
		if err != nil {
			log.Printf("noisereduce failed: %v — continuing with original input", err)
		} else {
			inputPathAbs = denoisedPath
			// caller should cleanup tmp files later (THE code already handles temp cleanup pattern)
		}
	} else {
		// For FFmpeg built-in filters: prefer arnndn (RNNoise) when requested and available.
		if dnMethod == "arnndn" || dnMethod == "rnnoise" {
			// Check that ffmpeg supports arnndn
			if ffmpegHasFilter("arnndn") {
				// RNNoise model path (make configurable)
				rnModel := os.Getenv("RNNOISE_MODEL_PATH")
				if rnModel == "" {
					rnModel = filepath.Join("tools", "models", "rnnoise-model.rnnn")
				}
				// If model file exists, set arnndn filter with model param; otherwise fall back.
				if _, err := os.Stat(rnModel); err == nil {
					// note: arnndn syntax: arnndn=m=path/to/model.rnnn
					denoiseFilter = fmt.Sprintf("arnndn=m=%s", rnModel)
				} else {
					log.Printf("arnndn requested but model not found at %s, falling back to afftdn", rnModel)
					denoiseFilter = "afftdn"
				}
			} else {
				log.Printf("arnndn filter not available in ffmpeg build, falling back to afftdn")
				denoiseFilter = "afftdn"
			}
		} else {
			// default: afftdn (broadband frequency-domain denoising)
			denoiseFilter = "afftdn"
		}
	}

	// 2) measure loudness (first pass)
	loudnessMap, _ := MeasureLoudness(ctx, inputPathAbs, opts.TargetLUFS)

	// 3) Build filter chain for second pass
	filterParts := []string{}
	if denoiseFilter != "" {
		filterParts = append(filterParts, denoiseFilter)
	}

	// preparing loudnorm application (using opts.TargetLUFS)
	loudnormApply := fmt.Sprintf("loudnorm=I=%v:TP=-1.5:LRA=7", opts.TargetLUFS)
	filterParts = append(filterParts, loudnormApply)
	// ---------------------------------------------------------------------
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

	stats.NoiseLevel = noiseLevel

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

func runNoisereduce(ctx context.Context, inputPath string, propDecrease float64, noiseSamplePath string) (string, error) {
	ffmpegPath, _ := exec.LookPath("ffmpeg") // used only if we need to resample (optional)
	_ = ffmpegPath

	tmpDir := os.TempDir()
	base := filepath.Base(inputPath)
	out := filepath.Join(tmpDir, fmt.Sprintf("nr_out_%d_%s.wav", time.Now().UnixNano(), base))

	// Build command: python tools/noisereduce_denoise.py --in <in> --out <out> [--noise <noise>] --prop-decrease <n>
	py, perr := exec.LookPath("python")
	if perr != nil {
		py, perr = exec.LookPath("python3")
	}
	if perr != nil {
		return "", fmt.Errorf("python not found in PATH (required for noisereduce helper)")
	}

	args := []string{"tools/noisereduce_denoise.py", "--in", inputPath, "--out", out, "--prop-decrease", fmt.Sprintf("%g", propDecrease)}
	if noiseSamplePath != "" {
		args = append(args[:len(args)-0], append([]string{"--noise", noiseSamplePath}, args[len(args):]...)...)
	}

	// run with a reasonable timeout
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(runCtx, py, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("noisereduce script failed: %w - stderr: %s", err, stderr.String())
	}

	// check file
	if _, err := os.Stat(out); os.IsNotExist(err) {
		return "", fmt.Errorf("noisereduce did not produce output %s", out)
	}
	return out, nil
}

func GetNoiseLevel(ctx context.Context, path string) (float64, error) {
	ffmpegPath, _ := exec.LookPath("ffmpeg")
	cmd := exec.CommandContext(ctx, ffmpegPath, "-i", path, "-af", "volumedetect", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// ffmpeg returns non-zero for null output; we will parse stderr anyway
	}
	out := stderr.String()
	// parse mean_volume: -xx.xx dB
	re := regexp.MustCompile(`mean_volume:\s*([-+]?[0-9]*\.?[0-9]+)\s*dB`)
	m := re.FindStringSubmatch(out)
	if len(m) < 2 {
		return 0, fmt.Errorf("mean_volume not found")
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v, nil
}
