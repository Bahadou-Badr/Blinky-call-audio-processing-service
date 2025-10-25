package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job represents a processing job record
type Job struct {
	ID         uuid.UUID  `json:"id"`
	InputPath  string     `json:"input_path"`
	OutputPath string     `json:"output_path"`
	Status     string     `json:"status"`
	Progress   int        `json:"progress"`
	ErrorMsg   *string    `json:"error_msg,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
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
		SELECT id, input_path, output_path, status, progress, error_msg, created_at, started_at, finished_at
		FROM audio_jobs WHERE id=$1
	`, id)
	var j Job
	var errMsg *string
	err := row.Scan(&j.ID, &j.InputPath, &j.OutputPath, &j.Status, &j.Progress, &errMsg, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return nil, err
	}
	j.ErrorMsg = errMsg
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
