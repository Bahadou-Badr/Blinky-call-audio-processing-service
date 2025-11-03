CREATE TABLE IF NOT EXISTS audio_jobs (
    id UUID PRIMARY KEY,
    input_path TEXT NOT NULL,
    output_path TEXT NOT NULL,
    status TEXT NOT NULL, -- queued | processing | done | failed
    progress INT DEFAULT 0,
    error_msg TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    started_at TIMESTAMP WITH TIME ZONE,
    finished_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_audio_jobs_status ON audio_jobs(status);