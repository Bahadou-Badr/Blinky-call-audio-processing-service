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
		log.Printf("enqueued job %s", jm.ID)
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

	// set started
	if err := st.SetStarted(ctx, jobUUID); err != nil {
		log.Printf("[w%d] db set started error: %v", workerID, err)
	}
	_ = st.UpdateProgress(ctx, jobUUID, 10)

	// processing options (could be paramized per-job later)
	opts := audio.ProcessOptions{
		DenoiseMethod: jm.DenoiseMethod,
		TargetLUFS:    -16.0,
		SampleRate:    16000,
		Channels:      1,
		UseCompressor: true,
		Compressor: audio.CompressorConf{
			ThresholdDB: -12,
			Ratio:       4,
			Attack:      100,
			Release:     1000,
		},
		UseLimiter: true,
		Limiter: audio.LimiterConf{
			ThresholdDB: -1.9,
		},
	}

	_ = st.UpdateProgress(ctx, jobUUID, 20)

	// set an adequate timeout for processing
	procCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	opts.DenoiseMethod = jm.DenoiseMethod
	start := time.Now()
	log.Printf("Processing job %s with denoise method: %s", jm.ID, jm.DenoiseMethod)
	stats, err := audio.ProcessFile(procCtx, jm.InputPath, jm.OutputPath, opts)
	if err != nil {
		log.Printf("[w%d] job %s failed: %v", workerID, jm.ID, err)
		_ = st.SetFailed(procCtx, jobUUID, err.Error())
		return
	}
	_ = st.UpdateProgress(procCtx, jobUUID, 70)

	// upload to S3
	objectKey := fmt.Sprintf("processed/%s", filepath.Base(jm.OutputPath))
	uploadCtx, cancelUpload := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelUpload()

	info, err := s3Client.UploadFile(uploadCtx, jm.OutputPath, objectKey, "audio/wav")
	if err != nil {
		log.Printf("[w%d] s3 upload failed for job %s: %v", workerID, jm.ID, err)
		_ = st.SetFailed(uploadCtx, jobUUID, "s3 upload failed: "+err.Error())
		return
	}

	// update DB with storage info
	versionID := info.VersionID
	if err := st.UpdateJobStorage(uploadCtx, jobUUID, s3Client.Bucket, objectKey, versionID); err != nil {
		log.Printf("[w%d] db update storage failed: %v", workerID, err)
		// continue; we still want to set done if everything else ok
	}

	// update metadata
	loudnessBytes, _ := json.Marshal(stats.Loudness)
	if stats.DurationSec > 0 {
		if err := st.UpdateJobMetadata(uploadCtx, jobUUID, stats.DurationSec, string(loudnessBytes)); err != nil {
			log.Printf("[w%d] db update metadata failed: %v", workerID, err)
		}
	} else {
		// still store loudness if duration missing
		if err := st.UpdateJobMetadata(uploadCtx, jobUUID, 0.0, string(loudnessBytes)); err != nil {
			log.Printf("[w%d] db update metadata failed: %v", workerID, err)
		}
	}

	// optional presign (we'll log it)
	presignedURL, err := s3Client.PresignedGetURL(uploadCtx, objectKey)
	if err != nil {
		log.Printf("[w%d] presign failed: %v", workerID, err)
	}

	_ = st.UpdateProgress(uploadCtx, jobUUID, 100)
	if err := st.SetFinished(uploadCtx, jobUUID); err != nil {
		log.Printf("[w%d] failed to mark finished: %v", workerID, err)
	}

	log.Printf("[w%d] job %s done in %s; s3=%s/%s ver=%s presign=%s", workerID, jm.ID, time.Since(start), s3Client.Bucket, objectKey, versionID, presignedURL)
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
