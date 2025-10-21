package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProcessOptions controls the pipeline and runtime limits.
type ProcessOptions struct {
	// "afftdn" or "rnnoise"
	DenoiseMethod string
	// target integrated loudness in LUFS (negative number)
	TargetLUFS float64
	// timeout for FFmpeg operations (used for defensive cancellation)
	Timeout time.Duration
}

// ProcessFile runs the processing chain and writes outputPath.
// Uses ffmpeg available in PATH. Returns an error on failure.
func ProcessFile(ctx context.Context, inputPath, outputPath string, opts ProcessOptions) error {
	if strings.TrimSpace(inputPath) == "" || strings.TrimSpace(outputPath) == "" {
		return errors.New("input or output path empty")
	}

	// ensure ffmpeg exists
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	// choose denoise filter
	var denoiseFilter string
	switch strings.ToLower(opts.DenoiseMethod) {
	case "rnnoise", "arnndn", "rnnd":
		// many ffmpeg builds expose "arnndn" for rnnoise
		denoiseFilter = "arnndn"
	default:
		denoiseFilter = "afftdn"
	}

	// build filter chain
	targetStr := strconv.FormatFloat(opts.TargetLUFS, 'f', -1, 64)
	filterChain := fmt.Sprintf("%s,loudnorm=I=%s:TP=-1.5:LRA=7", denoiseFilter, targetStr)

	// ffmpeg args: overwrite, input, audio filters, resample to 16k mono, remove video, output
	args := []string{
		"-y",
		"-i", inputPath,
		"-af", filterChain,
		"-ar", "16000",
		"-ac", "1",
		"-vn",
		outputPath,
	}

	// use provided ctx for cancellation (caller should set timeout)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// run command
	start := time.Now()
	if err := cmd.Run(); err != nil {
		// include stderr for debugging (Windows paths may appear there)
		return fmt.Errorf("ffmpeg failed after %s: %w - stderr: %s", time.Since(start), err, stderr.String())
	}
	return nil
}
