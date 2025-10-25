package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/audio"
	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/store"
)

type JobMsg struct {
	ID         string `json:"id"`
	InputPath  string `json:"input_path"`
	OutputPath string `json:"output_path"`
}

func main() {
	// flags
	natsURL := flag.String("nats", nats.DefaultURL, "nats url")
	pgConn := flag.String("db", "postgres://backdev:pa55word@localhost:5432/callaudio?sslmode=disable", "postgres conn string")
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

	// local job channel
	jobCh := make(chan JobMsg, 256)

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
		go worker(ctx, i, st, jobCh)
	}

	// graceful shutdown on SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
}

// worker processes jobs from jobCh
func worker(ctx context.Context, id int, st *store.Store, jobCh <-chan JobMsg) {
	log.Printf("[worker-%d] started", id)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[worker-%d] ctx done", id)
			return
		case jm := <-jobCh:
			processSingleJob(ctx, id, st, jm)
		}
	}
}

func processSingleJob(ctx context.Context, workerID int, st *store.Store, jm JobMsg) {
	// parse id
	jobUUID, err := uuid.Parse(jm.ID)
	if err != nil {
		log.Printf("[w%d] invalid job id: %v", workerID, err)
		return
	}
	// set started
	if err := st.SetStarted(ctx, jobUUID); err != nil {
		log.Printf("[w%d] db set started error: %v", workerID, err)
	}

	// update progress (10%)
	_ = st.UpdateProgress(ctx, jobUUID, 10)

	// call audio processing (wrap with timeout)
	procCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	opts := audio.ProcessOptions{
		DenoiseMethod: "afftdn", // or read from job metadata if extended
		TargetLUFS:    -16.0,
		SampleRate:    16000,
		Channels:      1,
		UseCompressor: true,
		Compressor: audio.CompressorConf{
			ThresholdDB: -12,
			Ratio:       4,
			Attack:      200,
			Release:     1000,
		},
		UseLimiter: true,
		Limiter: audio.LimiterConf{
			ThresholdDB: -1.5,
		},
	}

	// mark processing step
	_ = st.UpdateProgress(procCtx, jobUUID, 20)

	start := time.Now()
	stats, err := audio.ProcessFile(procCtx, jm.InputPath, jm.OutputPath, opts)
	if err != nil {
		log.Printf("[w%d] job %s failed: %v", workerID, jm.ID, err)
		_ = st.SetFailed(procCtx, jobUUID, err.Error())
		return
	}
	// update progress to 90
	_ = st.UpdateProgress(procCtx, jobUUID, 90)

	// mark finished
	if err := st.SetFinished(procCtx, jobUUID); err != nil {
		log.Printf("[w%d] failed to mark finished: %v", workerID, err)
	}

	log.Printf("[w%d] job %s done in %s (duration=%.2fs) stats=%v", workerID, jm.ID, time.Since(start), stats.DurationSec, stats.Loudness)
}
