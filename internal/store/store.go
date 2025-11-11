package store

import (
	"context"
	"database/sql"

	// "fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job represents a processing job record with storage/metadata fields
type Job struct {
	ID            uuid.UUID       `json:"id"`
	InputPath     string          `json:"input_path"`
	OutputPath    string          `json:"output_path"`
	Status        string          `json:"status"`
	Progress      int             `json:"progress"`
	ErrorMsg      *string         `json:"error_msg,omitempty"`
	S3Bucket      *string         `json:"s3_bucket,omitempty"`
	S3Key         *string         `json:"s3_key,omitempty"`
	S3Version     *string         `json:"s3_version_id,omitempty"`
	Duration      *float64        `json:"duration_sec,omitempty"`
	Loudness      sql.NullString  `json:"loudness_json,omitempty"`
	NoiseLevel    sql.NullFloat64 `json:"noise_level,omitempty"`
	DenoiseMethod *string         `json:"denoise_method,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(connStr string) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) CreateJob(ctx context.Context, inputPath, outputPath string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audio_jobs (id, input_path, output_path, status, created_at)
		VALUES ($1, $2, $3, 'queued', now())
	`, id, inputPath, outputPath)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) GetJob(ctx context.Context, id uuid.UUID) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, input_path, output_path, status, progress, error_msg, created_at, started_at, finished_at,
		       s3_bucket, s3_key, s3_version_id, duration_sec, loudness_json, noise_level, denoise_method
		FROM audio_jobs WHERE id=$1
	`, id)

	var j Job
	var errMsg *string
	var s3Bucket, s3Key, s3Version *string
	var duration sql.NullFloat64
	var loudnessJSON sql.NullString
	var noiseLevel sql.NullFloat64
	var denoiseMethod *string

	err := row.Scan(
		&j.ID, &j.InputPath, &j.OutputPath, &j.Status, &j.Progress, &errMsg,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt,
		&s3Bucket, &s3Key, &s3Version, &duration, &loudnessJSON, &noiseLevel, &denoiseMethod,
	)
	if err != nil {
		return nil, err
	}
	j.ErrorMsg = errMsg
	j.S3Bucket = s3Bucket
	j.S3Key = s3Key
	j.S3Version = s3Version
	if duration.Valid {
		val := duration.Float64
		j.Duration = &val
	}
	j.Loudness = loudnessJSON
	j.NoiseLevel = noiseLevel
	j.DenoiseMethod = denoiseMethod

	return &j, nil
}

func (s *Store) SetStarted(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE audio_jobs SET status='processing', started_at=now() WHERE id=$1`, id)
	return err
}

func (s *Store) UpdateProgress(ctx context.Context, id uuid.UUID, progress int) error {
	_, err := s.pool.Exec(ctx, `UPDATE audio_jobs SET progress=$2 WHERE id=$1`, id, progress)
	return err
}

func (s *Store) SetFinished(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE audio_jobs SET status='done', progress=100, finished_at=now() WHERE id=$1`, id)
	return err
}

func (s *Store) SetFailed(ctx context.Context, id uuid.UUID, msg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE audio_jobs SET status='failed', error_msg=$2, finished_at=now() WHERE id=$1`, id, msg)
	return err
}

// UpdateJobStorage sets s3 bucket/key/version for a job
func (s *Store) UpdateJobStorage(ctx context.Context, id uuid.UUID, bucket, key, versionID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE audio_jobs SET s3_bucket=$2, s3_key=$3, s3_version_id=$4 WHERE id=$1
	`, id, bucket, key, versionID)
	return err
}

// UpdateJobMetadata sets duration and loudness json
func (s *Store) UpdateJobMetadata(ctx context.Context, id uuid.UUID, duration float64, loudnessJSON string, noiseLevel float64, denoiseMethod string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE audio_jobs SET duration_sec=$2, loudness_json=$3, noise_level=$4, denoise_method=$5 WHERE id=$1
	`, id, duration, loudnessJSON, noiseLevel, denoiseMethod)
	return err
}
