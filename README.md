# Blinky-call-audio-processing-service
A Go-based backend that denoises and normalizes call recordings for consistent loudness and clarity. The pipeline uses FFmpeg filters (e.g. ``afftdn`` for FFT denoising and ``loudnorm`` for EBU R128 loudness normalization) and an optional Python-based AI denoiser (``noisereduce``). Processed audio can be used for transcription, QA tools, etc. The system supports both batch and real-time workflows via REST APIs and a job queue.

### Key Features
- **Noise Reduction**: Frequency-domain FFT denoising with FFmpeg’s ``afftdn`` (reduces broadband noise). Optionally applies a spectral-gating denoiser via the noisereduce Python library.
- **Loudness Normalization**: Two-pass EBU R128 normalization using FFmpeg’s ``loudnorm`` filter. Ensures all files meet a target LUFS level.
- **Optional Compression/Limiting**: After normalization, an FFmpeg limiter/compressor is applied to catch peaks (configurable thresholds).
- **Configurable Pipeline**: Filter chain and parameters (noise reduction method, target LUFS, etc.) are driven by a YAML config or JSON options.
- **Distributed Processing**: Audio jobs are enqueued to NATS and handled by concurrent worker services. Each job’s metadata (status, timestamps, etc.) is stored in PostgreSQL.
- **Storage & Metadata**: Processed files are saved to MinIO(S3-compatible object storage ). Public/private URLs and metadata (duration, loudness, SNR) are recorded in the database.
- **Observability**: Exposes Prometheus metrics (job latencies, error counts) for Grafana dashboards. Audio quality (e.g. estimated SNR before/after) is computed and logged for each job.

### Tech Stack

- **Language**: Go (backend API & worker) + Python (noise reduction helper).
- **FFmpeg**: Called via exec.Command to apply filters.
- **Message Queue**: NATS (publish/subscribe) for job distribution.
- **Database**: PostgreSQL for job state and metadata.
- **Storage**: MinIO (S3-compatible) for input/output audio files.
- **Monitoring**: Prometheus + Grafana for service metrics.

### Setup
- **Prerequisites**: Install Go, Python3, FFmpeg, and (optionally) Docker. Ensure Python can pip install noisereduce.
- **Clone & Build**:

```bash
git clone https://github.com/Bahadou-Badr/Blinky-call-audio-processing-service.git
cd Blinky-call-audio-processing-service
go build ./cmd/api && go build ./cmd/worker

```
- **Configure**:Copy ``config.yaml`` and adjust settings: database DSN, NATS URL, storage bucket names, target LUFS, etc. Place the RNNoise model file if using FFmpeg’s ``arnndn`` (not required by default).
- **Run Services**: Start dependent services (PostgreSQL, NATS server, MinIO). Then run the API and worker executables (or use Docker/Docker Compose if set up).
- **Health Check**: The API exposes ``/health`` (returns “ok”) to verify it’s running.

### Usage Examples
- **Submit a Job**: POST an audio file to ``/submit`` (multipart form “file”). For example:
```bash
curl -X POST -F "file=@/path/to/call.wav" http://localhost:8080/submit -F "denoise_method=noisereduce"
```
![u](/screenshots/output_return.png)
Returns JSON with a job ID.
- **Check Status**: Poll ``/status/{id}`` to get job progress and metadata. E.g.:
```bash
curl http://localhost:8080/status/your-job-uuid
```
![u](/screenshots/job_status.png)
- **Process File (Sync mode)**: (Phase 1 prototype) The ``/process`` endpoint accepted an upload and returned the processed file immediately. In later phases ``/process`` was superseded by the async ``/submit``/``/status`` model.

- **Retrieve Result**: When a job completes, the response includes a URL (MinIO link) to download the denoised/normalized audio.
![u](/screenshots/minio.png)

### Pipeline Overview
```
                 ┌──────────────────────┐
                 │      Client App      │
                 │  (Uploads Audio)     │
                 └──────────┬───────────┘
                            │ 1. Upload
                            ▼
                 ┌──────────────────────────┐
                 │        API Server        │
                 │ - Validate file          │
                 │ - Save temp file         │
                 │ - Create Job ID (UUID)   │
                 │ - Publish to NATS        │
                 └──────────┬───────────────┘
                            │ 2. Publish Job
                            ▼
                 ┌──────────────────────────┐
                 │         NATS MQ          │
                 │   (Job Queue / Stream)   │
                 └──────────┬───────────────┘
                            │ 3. Worker pull job
                            ▼
             ┌──────────────────────────────────┐
             │            Worker(s)             │
             │ 1. Download/Read input audio     │
             │ 2. Apply Denoising:              │
             │    - FFmpeg afftdn OR            │
             │    - AI-based Noisereduce        │
             │ 3. Apply Loudness Normalization  │
             │    (FFmpeg loudnorm)             │
             │ 4. Compressor/Limiter            │
             │ 5. Generate metadata (LUFS, etc) │
             └──────────┬───────────────────────┘
                        │ 4. Upload
                        ▼
          ┌─────────────────────────────────────┐
          │       S3 / MinIO Storage Bucket     │
          │ - Store processed output file       │
          │ - Generate file URL                 │
          └──────────┬──────────────────────────┘
                     │ 5. Update metadata
                     ▼
          ┌─────────────────────────────────────┐
          │             PostgreSQL              │
          │ - Job status (pending→done)         │
          │ - File path / S3 URL                │
          │ - Processing timestamps             │
          │ - Audio stats (LUFS, SNR, etc.)     │
          └──────────┬──────────────────────────┘
                     │ 6. Poll status
                     ▼
          ┌─────────────────────────────────────┐
          │              API Server             │
          │ Returns:                            │
          │ - jobId                             │
          │ - status (pending/processing/done)  │
          │ - file URL when ready               │
          └─────────────────────────────────────┘
```


### Technologies Cited
- noisereduce (Python): A spectral-gating noise reduction library for audio.
- FFmpeg filters: ``afftdn`` (FFT-based denoising), ``loudnorm`` (EBU R128 normalization).
- MinIO (S3 Storage): High-performance, S3-compatible object store.

This mix of Go concurrency, FFmpeg audio processing, and modern cloud tech makes Blinky a robust backend for call recording cleanup.