package storage

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Client wraps minio client + bucket config
type S3Client struct {
	Client        *minio.Client
	Bucket        string
	PresignExpiry time.Duration
}

// S3Config holds configuration (Endpoint can be "localhost:9000" or "http://localhost:9000")
type S3Config struct {
	Endpoint    string
	AccessKey   string
	SecretKey   string
	Bucket      string
	UseSSL      bool // optional override: if true/false, it forces Secure. If false and Endpoint has scheme, scheme takes precedence.
	PresignSecs int
}

// normalizeEndpoint accepts either "localhost:9000" or "http://localhost:9000" (with or without trailing slash)
// and returns host:port and secure flag. It strips any path component and ignores trailing slashes.
func normalizeEndpoint(raw string, cfgUseSSL bool) (endpointHost string, secure bool, err error) {
	if raw == "" {
		return "", false, errors.New("empty endpoint")
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")

	// If raw contains scheme, parse it
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", false, perr
		}
		if u.Host == "" {
			return "", false, errors.New("invalid endpoint URL (missing host)")
		}
		secure = (u.Scheme == "https")
		return u.Host, secure, nil
	}

	// If user passed something like "http:localhost:9000" (invalid), try parse anyway
	u, perr := url.Parse("http://" + raw)
	if perr == nil && u.Host != "" && u.Path == "" {
		// treat as non-SSL by default, but allow cfgUseSSL to override
		return u.Host, cfgUseSSL, nil
	}

	// fallback: assume raw is host:port
	// no scheme present — use cfgUseSSL as secure flag
	return raw, cfgUseSSL, nil
}

// NewS3Client creates and returns S3Client. Endpoint may be "localhost:9000" or "http://localhost:9000"
func NewS3Client(cfg S3Config) (*S3Client, error) {
	endpointHost, secure, err := normalizeEndpoint(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, err
	}

	// If cfg.UseSSL was explicitly set to true or false, preserve it (normalizeEndpoint uses it when no scheme provided)
	// Create minio client using host:port (no path, no scheme)
	minioClient, err := minio.New(endpointHost, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	// ensure bucket exists (create if not) — helpful in development
	exists, err := minioClient.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := minioClient.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, err
		}
	}

	exp := time.Duration(cfg.PresignSecs) * time.Second
	return &S3Client{
		Client:        minioClient,
		Bucket:        cfg.Bucket,
		PresignExpiry: exp,
	}, nil
}

// UploadFile uploads a local file to S3 and returns upload info
func (s *S3Client) UploadFile(ctx context.Context, localPath, objectKey, contentType string) (minio.UploadInfo, error) {
	info, err := s.Client.FPutObject(ctx, s.Bucket, objectKey, localPath, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return minio.UploadInfo{}, err
	}
	return info, nil
}

// PresignedGetURL returns a presigned GET URL for the objectKey valid for PresignExpiry
func (s *S3Client) PresignedGetURL(ctx context.Context, objectKey string) (string, error) {
	params := url.Values{}
	u, err := s.Client.PresignedGetObject(ctx, s.Bucket, objectKey, s.PresignExpiry, params)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
