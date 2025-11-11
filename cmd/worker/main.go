package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/audio"
	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/metrics"
	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/storage"
	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/store"
)

type JobMsg struct {
	ID            string `json:"id"`
	InputPath     string `json:"input_path"`
	OutputPath    string `json:"output_path"`
	DenoiseMethod string `json:"denoise_method"`
}

func main() {
	// flags
	natsURL := flag.String("nats", nats.DefaultURL, "nats url")
	pgConn := flag.String("db", "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable", "postgres conn string")
	concurrency := flag.Int("concurrency", runtime.NumCPU(), "number of worker goroutines")
	flag.Parse()

	// init store
	st, err := store.New(*pgConn)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer st.Close()

	// connect NATS
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	// init S3 client (MinIO)
	s3Cfg := storage.S3Config{
		Endpoint:    env("S3_ENDPOINT", "http://localhost:9000"),
		AccessKey:   env("S3_ACCESS_KEY", "miniouser"),
		SecretKey:   env("S3_SECRET_KEY", "miniopass"),
		Bucket:      env("S3_BUCKET", "call-audio-bucket"),
		UseSSL:      false,
		PresignSecs: int(getIntEnv("S3_PRESIGN_SECS", 60*60*24*7)),
	}
	s3Client, err := storage.NewS3Client(s3Cfg)
	if err != nil {
		log.Fatalf("s3 init: %v", err)
	}

	// local job channel
	jobCh := make(chan JobMsg, 512)

	// subscribe to audio.jobs subject
	_, err = nc.Subscribe("audio.jobs", func(msg *nats.Msg) {
		var jm JobMsg
		if err := json.Unmarshal(msg.Data, &jm); err != nil {
			log.Printf("invalid job msg: %v", err)
			return
		}
		log.Printf("enqueued job %s (denoiser=%s)", jm.ID, jm.DenoiseMethod)
		jobCh <- jm
	})
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	// worker pool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < *concurrency; i++ {
		go worker(ctx, i, st, s3Client, jobCh)
	}

	// graceful shutdown on SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
}

func worker(ctx context.Context, id int, st *store.Store, s3Client *storage.S3Client, jobCh <-chan JobMsg) {
	log.Printf("[worker-%d] started", id)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[worker-%d] ctx done", id)
			return
		case jm := <-jobCh:
			processSingleJob(ctx, id, st, s3Client, jm)
		}
	}
}

func processSingleJob(ctx context.Context, workerID int, st *store.Store, s3Client *storage.S3Client, jm JobMsg) {
	jobUUID, err := uuid.Parse(jm.ID)
	if err != nil {
		log.Printf("[w%d] invalid job id: %v", workerID, err)
		return
	}

	if err := st.SetStarted(ctx, jobUUID); err != nil {
		log.Printf("[w%d] db set started error: %v", workerID, err)
	}
	_ = st.UpdateProgress(ctx, jobUUID, 10)

	opts := audio.ProcessOptions{
		DenoiseMethod: jm.DenoiseMethod,
		TargetLUFS:    -16.0,
		SampleRate:    48000,
		Channels:      1,
		UseCompressor: true,
		Compressor: audio.CompressorConf{
			ThresholdDB: -20,
			Ratio:       3.1,
			Attack:      5,
			Release:     120,
		},
		UseLimiter: true,
		Limiter: audio.LimiterConf{
			ThresholdDB: -1.0,
		},
	}

	_ = st.UpdateProgress(ctx, jobUUID, 20)

	procCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	snrCtx, cancelSnr := context.WithTimeout(ctx, 90*time.Second)
	defer cancelSnr()

	// Estimate SNR before
	snrBeforeMetrics, err := audio.EstimateQuality(snrCtx, jm.InputPath)
	if err != nil {
		log.Printf("[w%d] warning: SNR before estimation failed for job %s: %v", workerID, jm.ID, err)
	}
	snrBefore := 0.0
	if snrBeforeMetrics != nil {
		snrBefore = snrBeforeMetrics.SNR
	}

	loudBeforeMap, _ := audio.MeasureLoudness(procCtx, jm.InputPath, opts.TargetLUFS)

	start := time.Now()
	log.Printf("Processing job %s with denoise method: %s", jm.ID, jm.DenoiseMethod)

	stats, err := audio.ProcessFile(procCtx, jm.InputPath, jm.OutputPath, opts)
	if err != nil {
		log.Printf("[w%d] job %s failed: %v", workerID, jm.ID, err)
		_ = st.SetFailed(procCtx, jobUUID, err.Error())
		return
	}

	// Estimate SNR after
	snrAfterMetrics, err := audio.EstimateQuality(snrCtx, jm.OutputPath)
	if err != nil {
		log.Printf("[w%d] warning: SNR after estimation failed for job %s: %v", workerID, jm.ID, err)
	}
	snrAfter := 0.0
	if snrAfterMetrics != nil {
		snrAfter = snrAfterMetrics.SNR
	}

	loudAfterMap, _ := audio.MeasureLoudness(procCtx, jm.OutputPath, opts.TargetLUFS)

	_ = st.UpdateProgress(procCtx, jobUUID, 70)

	objectKey := fmt.Sprintf("processed/%s", filepath.Base(jm.OutputPath))
	uploadCtx, cancelUpload := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelUpload()

	info, err := s3Client.UploadFile(uploadCtx, jm.OutputPath, objectKey, "audio/wav")
	if err != nil {
		log.Printf("[w%d] s3 upload failed for job %s: %v", workerID, jm.ID, err)
		_ = st.SetFailed(uploadCtx, jobUUID, "s3 upload failed: "+err.Error())
		return
	}

	versionID := info.VersionID
	if err := st.UpdateJobStorage(uploadCtx, jobUUID, s3Client.Bucket, objectKey, versionID); err != nil {
		log.Printf("[w%d] db update storage failed: %v", workerID, err)
	}

	loudnessBytes, _ := json.Marshal(stats.Loudness)
	if stats.DurationSec > 0 {
		_ = st.UpdateJobMetadata(uploadCtx, jobUUID, stats.DurationSec, string(loudnessBytes), stats.NoiseLevel, jm.DenoiseMethod)
	} else {
		_ = st.UpdateJobMetadata(uploadCtx, jobUUID, 0.0, string(loudnessBytes), stats.NoiseLevel, jm.DenoiseMethod)
	}

	presignedURL, err := s3Client.PresignedGetURL(uploadCtx, objectKey)
	if err != nil {
		log.Printf("[w%d] presign failed: %v", workerID, err)
	}

	_ = st.UpdateProgress(uploadCtx, jobUUID, 100)
	_ = st.SetFinished(uploadCtx, jobUUID)

	var loudBefore, loudAfter float64
	if v, ok := loudBeforeMap["input_i"]; ok {
		loudBefore = v
	}
	if v, ok := loudAfterMap["input_i"]; ok {
		loudAfter = v
	}

	duration := time.Since(start)
	metrics.ObserveJob(jm.DenoiseMethod, duration, err == nil, loudBefore, loudAfter, snrBefore, snrAfter)

	log.Printf("[w%d] job %s done in %s; s3=%s/%s ver=%s presign=%s snr_before=%.2f snr_after=%.2f",
		workerID, jm.ID, duration, s3Client.Bucket, objectKey, versionID, presignedURL, snrBefore, snrAfter)
}

// helpers for env
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func getIntEnv(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i
		}
	}
	return d
}
