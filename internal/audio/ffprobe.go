package audio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// GetDuration returns duration in seconds (float) via ffprobe
func GetDuration(ctx context.Context, path string) (float64, error) {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, fmt.Errorf("ffprobe not found in PATH: %w", err)
	}
	args := []string{"-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path}
	cmd := exec.CommandContext(ctx, ffprobePath, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w - stderr: %s", err, stderr.String())
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return 0, errors.New("ffprobe returned empty duration")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %w", err)
	}
	return f, nil
}

// LoudnessStats holds measured loudnorm stats
type LoudnessStats struct {
	InputI         float64 // measured integrated loudness
	InputTP        float64
	InputLRA       float64
	InputThresh    float64
	MeasuredI      float64
	MeasuredTP     float64
	MeasuredLRA    float64
	MeasuredThresh float64
	// raw lines
	Raw map[string]string
}

// MeasureLoudness runs ffmpeg single-pass loudnorm with print_format=summary and parses key metrics
// It returns a map with measured values (I, TP, LRA, threshold) from FFmpeg output.
func MeasureLoudness(ctx context.Context, path string, targetLufs float64) (map[string]float64, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}

	// Use loudnorm with print_format=summary; single-pass measure only
	// Example: ffmpeg -i input.wav -af loudnorm=I=-16:TP=-1.5:LRA=7:print_format=summary -f null -
	args := []string{"-i", path, "-af", fmt.Sprintf("loudnorm=I=%v:TP=-1.5:LRA=7:print_format=summary", targetLufs), "-f", "null", "-"}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// ffmpeg prints summary to stderr
	if err := cmd.Run(); err != nil {
		// ffmpeg returns non-zero when output is null; still parse stderr
		// we'll attempt to parse stderr even on error
	}

	out := stderr.String()
	return parseLoudnormSummary(out)
}

// parseLoudnormSummary reads the ffmpeg loudnorm summary and returns a map of measured values
func parseLoudnormSummary(s string) (map[string]float64, error) {
	// Example snippet contains lines like:
	// Input Integrated:    -24.8 LUFS
	// Input True Peak:     -0.3 dBTP
	// Input LRA:           4.0 LU
	// ...
	// Target offset: 7.0 LU
	// Output Integrated:   -16.0 LUFS

	result := map[string]float64{}
	reFloat := regexp.MustCompile(`(-?\d+(\.\d+)?)`)

	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Input Integrated:"):
			if m := reFloat.FindString(line); m != "" {
				if v, err := strconv.ParseFloat(m, 64); err == nil {
					result["input_i"] = v
				}
			}
		case strings.HasPrefix(line, "Input True Peak:"):
			if m := reFloat.FindString(line); m != "" {
				if v, err := strconv.ParseFloat(m, 64); err == nil {
					result["input_tp"] = v
				}
			}
		case strings.HasPrefix(line, "Input LRA:"):
			if m := reFloat.FindString(line); m != "" {
				if v, err := strconv.ParseFloat(m, 64); err == nil {
					result["input_lra"] = v
				}
			}
		case strings.HasPrefix(line, "Output Integrated:"):
			if m := reFloat.FindString(line); m != "" {
				if v, err := strconv.ParseFloat(m, 64); err == nil {
					result["output_i"] = v
				}
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("failed to parse loudnorm summary (no metrics found)")
	}
	return result, nil
}
