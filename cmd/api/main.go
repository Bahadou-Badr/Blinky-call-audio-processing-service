package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/audio"
)

const (
	storageInputDir  = "storage/input"
	storageOutputDir = "storage/output"
	maxUploadSize    = 200 << 20 // 200 MB
	// FFmpeg command timeout per file
	ffmpegTimeout = 90 * time.Second
)

func main() {
	// Ensure folders exist (works on Windows)
	mustMkdir(storageInputDir)
	mustMkdir(storageOutputDir)

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/process", processHandler)

	addr := ":8080"
	log.Printf("Starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// /process: multipart form upload
// fields:
// - file: audio file
// - denoise (optional): "afftdn" (default) or "rnnoise"
func processHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// protect against huge uploads
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "failed to parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	denoiseMethod := r.FormValue("denoise")
	if denoiseMethod == "" {
		denoiseMethod = "afftdn"
	}

	// generate safe input and output paths
	ts := time.Now().UnixNano()
	cleanName := sanitizeFilename(fh.Filename)
	inputFilename := fmt.Sprintf("%d_%s", ts, cleanName)
	inputPath := filepath.Join(storageInputDir, inputFilename)
	outFilename := inputFilename + "_processed.wav"
	outputPath := filepath.Join(storageOutputDir, outFilename)

	// save incoming file to disk
	inFile, err := os.Create(inputPath)
	if err != nil {
		http.Error(w, "failed to create input file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(inFile, file); err != nil {
		inFile.Close()
		http.Error(w, "failed to write input file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	inFile.Close()

	// create context with timeout for processing
	pctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()

	opts := audio.ProcessOptions{
		DenoiseMethod: denoiseMethod,
		TargetLUFS:    -16.0,
		Timeout:       ffmpegTimeout,
	}

	// synchronous processing for Phase 1
	start := time.Now()
	if err := audio.ProcessFile(pctx, inputPath, outputPath, opts); err != nil {
		http.Error(w, "processing failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	elapsed := time.Since(start)

	resp := map[string]interface{}{
		"input":      inputPath,
		"output":     outputPath,
		"denoise":    denoiseMethod,
		"elapsed_ms": elapsed.Milliseconds(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// helpers
func mustMkdir(path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		log.Fatalf("failed to create dir %s: %v", path, err)
	}
}

func sanitizeFilename(name string) string {
	// basic clean: drop directories and keep base name
	return filepath.Base(name)
}
