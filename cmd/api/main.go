package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/storage"
	"github.com/Bahadou-Badr/Blinky-call-audio-processing-service/internal/store"
)

const (
	storageInputDir  = "storage/input"
	storageOutputDir = "storage/output"
	maxUploadSize    = 300 << 20 // 300 MB
)

func main() {
	// config from env (simple)
	natsURL := env("NATS_URL", nats.DefaultURL)
	pgConn := env("DATABASE_URL", "postgres://backdev:pa55word@localhost:5432/callaudio?sslmode=disable")
	addr := env("HTTP_ADDR", ":8080")

	// create storage dirs (input + output)
	if err := os.MkdirAll(storageInputDir, 0o755); err != nil {
		log.Fatalf("mkdir input: %v", err)
	}
	if err := os.MkdirAll(storageOutputDir, 0o755); err != nil {
		log.Fatalf("mkdir output: %v", err)
	}

	// connect to store (Postgres)
	st, err := store.New(pgConn)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer st.Close()

	// connect to NATS
	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	// init S3 client for presign (API will use presigned URLs)
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

	server := &APIServer{
		store: st,
		nc:    nc,
		s3:    s3Client,
	}

	http.HandleFunc("/health", server.health)
	http.HandleFunc("/submit", server.submitHandler)
	http.HandleFunc("/status/", server.statusHandler) // expects /status/{uuid}

	log.Printf("API listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

type APIServer struct {
	store *store.Store
	nc    *nats.Conn
	s3    *storage.S3Client
}

func (s *APIServer) health(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

// submitHandler: multipart upload field "file"
func (s *APIServer) submitHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()

	denoiseMethod := r.FormValue("denoise_method")
	if denoiseMethod == "" {
		denoiseMethod = "none" // default
	}
	// persist input file
	ts := time.Now().UnixNano()
	filename := fmt.Sprintf("%d_%s", ts, sanitize(fh.Filename))
	outFilename := filename + "_processed.wav"
	inputPath := filepath.Join(storageInputDir, filename)
	out, err := os.Create(inputPath)
	if err != nil {
		http.Error(w, "create file error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		http.Error(w, "write file error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out.Close()

	// create job in DB (output path default)
	outputPath := filepath.Join(storageOutputDir, outFilename)
	jobID, err := s.store.CreateJob(ctx, inputPath, outputPath)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// publish to NATS subject
	msg := map[string]string{
		"id":             jobID.String(),
		"input_path":     inputPath,
		"output_path":    outputPath,
		"denoise_method": denoiseMethod,
	}
	b, _ := json.Marshal(msg)
	if err := s.nc.Publish("audio.jobs", b); err != nil {
		// log but continue: worker may poll DB later
		log.Printf("nats publish error: %v", err)
	}

	log.Printf("enqueued job %s (method=%s)", jobID.String(), denoiseMethod)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID.String()})
}

func (s *APIServer) statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// path: /status/{id}
	idStr := filepath.Base(r.URL.Path)
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	job, err := s.store.GetJob(ctx, id)
	if err != nil {
		http.Error(w, "not found: "+err.Error(), http.StatusNotFound)
		return
	}

	resp := map[string]interface{}{
		"job": job,
	}

	// generating presigned url, if we have s3 key
	if job.S3Key != nil && *job.S3Key != "" {
		presigned, err := s.s3.PresignedGetURL(ctx, *job.S3Key)
		if err == nil {
			resp["presigned_url"] = presigned
		} else {
			// fallback to bucket/key for debugging
			resp["s3_ref"] = fmt.Sprintf("%s/%s", deref(job.S3Bucket), deref(job.S3Key))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func env(k, d string) string {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	return v
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

func sanitize(name string) string {
	return filepath.Base(name)
}
