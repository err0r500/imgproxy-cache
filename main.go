package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	S3Bucket           string
	S3Folder           string
	TigrisProxyBind    string
	HealthCheckTimeout time.Duration
}

func main() {
	healthCheckTimeout := 30 * time.Second
	if os.Getenv("HEALTH_CHECK_TIMEOUT_IN_SEC") != "" {
		t, err := strconv.ParseInt(os.Getenv("HEALTH_CHECK_TIMEOUT_IN_SEC"), 10, 64)
		if err != nil {
			slog.Error("Failed to parse HEALTH_CHECK_TIMEOUT_IN_SEC", "error", err)
			os.Exit(1)
		}
		healthCheckTimeout = time.Duration(t) * time.Second
	}

	cfg := Config{
		S3Bucket:           os.Getenv("S3_BUCKET"),
		S3Folder:           os.Getenv("S3_FOLDER"),
		TigrisProxyBind:    os.Getenv("IMGPROXY_BIND"),
		HealthCheckTimeout: healthCheckTimeout,
	}
	if cfg.S3Bucket == "" {
		slog.Error("Missing required environment variable(s)", "config", cfg)
		os.Exit(1)
	}
	if cfg.TigrisProxyBind == "" {
		cfg.TigrisProxyBind = ":8080"
	}

	// Initialize S3 uploader
	uploader := manager.NewUploader(initS3Client(), func(u *manager.Uploader) {
		u.PartSize = 5 * 1024 * 1024
		u.BufferProvider = manager.NewBufferedReadSeekerWriteToPool(10 * 1024 * 1024)
	})

	// Initialize the proxy
	targetURL := "http://127.0.0.1:8081"
	target, err := url.Parse(targetURL)
	if err != nil {
		slog.Error("Failed to parse imgproxy local endpoint", "error", err)
		os.Exit(1)
	}

	// Wait for the health endpoint to be ready
	slog.Info("Waiting for imgproxy to be ready...")
	if err := waitForHealth(targetURL, cfg.HealthCheckTimeout); err != nil {
		slog.Error("Health check failed", "error", err)
		os.Exit(1)
	}
	slog.Info("imgproxy is ready")

	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusOK {
			// Read the entire response body into a buffer
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				slog.Error("Failed to read response body", "error", err)
				return err
			}

			// Create two separate readers from the same bytes
			// One for the response and one for S3 upload
			respReader := bytes.NewReader(bodyBytes)
			s3Reader := bytes.NewReader(bodyBytes)

			// Replace the response body with our buffered copy
			resp.Body = io.NopCloser(respReader)

			// Use RawPath if available (preserves URL encoding), otherwise use Path
			originalPath := resp.Request.URL.Path
			if resp.Request.URL.RawPath != "" {
				originalPath = resp.Request.URL.RawPath
			}

			// Upload the complete file to S3 in a goroutine
			go func() {
				if err := uploadToS3(context.Background(), uploader, cfg, s3Reader, originalPath); err != nil {
					slog.Error("S3 upload failed", "error", err)
				}
			}()
		}
		return nil
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	if err := http.ListenAndServe(fmt.Sprintf("%s", cfg.TigrisProxyBind), nil); err != nil {
		slog.Error("Server failed", "error", err)
	}
}

func uploadToS3(ctx context.Context, uploader *manager.Uploader, cfg Config, r io.Reader, path string) error {
	key := GenerateS3Key(path)

	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(fmt.Sprintf("%s%s", cfg.S3Folder, key)),
		Body:   r,
	})

	if err != nil {
		slog.Error("Upload failed", "path", path, "key", key, "error", err)
		return err
	}

	slog.Info("Uploaded to S3", "path", path, "bucket", cfg.S3Bucket, "key", key)
	return nil
}

// GenerateS3Key creates a hash from the imgproxy URL path
func GenerateS3Key(path string) string {
	hash := md5.Sum([]byte(path))
	return hex.EncodeToString(hash[:])
}

func initS3Client() *s3.Client {
	sdkConfig, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		slog.Error("Failed to initialize AWS config", "error", err)
		os.Exit(1)
	}

	svc := s3.NewFromConfig(sdkConfig, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(getEnvWithDefault("S3_ENDPOINT", "https://fly.storage.tigris.dev"))
		o.UsePathStyle = true
		o.Region = "auto"
	})

	return svc
}

func getEnvWithDefault(key, defaultValue string) string {
	env, ok := os.LookupEnv(key)
	if !ok {
		return defaultValue
	}
	return env
}

func waitForHealth(target string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	endTime := time.Now().Add(timeout)

	for time.Now().Before(endTime) {
		resp, err := client.Get(fmt.Sprintf("%s/health", target))
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("health check failed after %v", timeout)
}
