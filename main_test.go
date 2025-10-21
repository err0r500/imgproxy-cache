package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	minioAccessKey  = "minioadmin"
	minioSecretKey  = "minioadmin"
	sourceBucket    = "source-images"
	processedBucket = "processed-images"
	sourceKey       = "kitten.jpg"
)

// TestFullEndToEnd tests the complete workflow:
func TestFullEndToEnd(t *testing.T) {
	ctx := context.Background()

	dockerNetwork := createDockerNetwork(t, ctx)
	defer func() {
		if err := dockerNetwork.Remove(ctx); err != nil {
			t.Logf("Failed to remove network: %v", err)
		}
	}()

	minioContainer, minioEndpoint, minioInternalEndpoint := setupMinIOContainerWithNetwork(t, ctx, dockerNetwork)
	defer func() {
		if err := minioContainer.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate MinIO container: %v", err)
		}
	}()

	testImageData, err := os.ReadFile("kitten.jpg")
	if err != nil {
		t.Fatalf("Failed to read test image: %v", err)
	}

	s3Client := minIOClient(t, minioEndpoint)
	createBucketsAndUploadSourceImage(ctx, s3Client, testImageData, t)
	ensureImageIsPubliclyReachable(t, minioEndpoint, testImageData)

	internalImageURL := fmt.Sprintf("%s/%s/%s", minioInternalEndpoint, sourceBucket, sourceKey)

	proxyContainer, proxyURL := startProxyContainer(t, ctx, dockerNetwork, minioInternalEndpoint)
	defer func() {
		if err := proxyContainer.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate proxy container: %v", err)
		}
	}()

	// URL-encode the source image URL for imgproxy's "plain" format
	imgproxyPath := "/_/rs:fill:50:50/plain/" + url.QueryEscape(internalImageURL)

	processedImageData := requestImageThroughProxy(t, fmt.Sprintf("%s%s", proxyURL, imgproxyPath))
	if len(processedImageData) == 0 {
		t.Fatal("Received empty image data")
	}

	storedImageData := getImageFromCacheBucket(t, ctx, s3Client, imgproxyPath)
	if !bytes.Equal(processedImageData, storedImageData) {
		t.Fatal("Stored image does not match response image")
	}

	t.Log("✓ Source image fetched from MinIO")
	t.Log("✓ Image processed by imgproxy")
	t.Log("✓ Processed image uploaded to MinIO")
	t.Log("✓ Response matches stored image")
}

func setupMinIOContainerWithNetwork(t *testing.T, ctx context.Context, network *testcontainers.DockerNetwork) (testcontainers.Container, string, string) {
	minioAlias := "minio"
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:latest",
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioAccessKey,
				"MINIO_ROOT_PASSWORD": minioSecretKey,
			},
			Cmd:            []string{"server", "/data"},
			Networks:       []string{network.Name},
			NetworkAliases: map[string][]string{network.Name: {minioAlias}},
			WaitingFor:     wait.ForHTTP("/minio/health/live").WithPort("9000").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("Failed to start MinIO container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "9000")
	if err != nil {
		t.Fatalf("Failed to get container port: %v", err)
	}

	// External endpoint for host to access
	externalEndpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	// Internal endpoint for container-to-container communication
	internalEndpoint := fmt.Sprintf("http://%s:9000", minioAlias)

	return container, externalEndpoint, internalEndpoint
}

func minIOClient(t *testing.T, endpoint string) *s3.Client {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			minioAccessKey,
			minioSecretKey,
			"",
		)),
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return client
}

func createDockerNetwork(t *testing.T, ctx context.Context) *testcontainers.DockerNetwork {
	network, err := network.New(ctx)
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}
	return network
}

func createBucketsAndUploadSourceImage(ctx context.Context, s3Client *s3.Client, testImageData []byte, t *testing.T) {
	for _, bucket := range []string{sourceBucket, processedBucket} {
		_, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			t.Fatalf("Failed to create bucket %s: %v", bucket, err)
		}
		t.Logf("Created bucket: %s", bucket)
	}

	// Make source bucket public for read access
	publicPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": ["*"]},
			"Action": ["s3:GetObject"],
			"Resource": ["arn:aws:s3:::%s/*"]
		}]
	}`, sourceBucket)

	_, err := s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(sourceBucket),
		Policy: aws.String(publicPolicy),
	})
	if err != nil {
		t.Fatalf("Failed to set bucket policy: %v", err)
	}
	t.Logf("Made bucket %s publicly readable", sourceBucket)

	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(sourceBucket),
		Key:    aws.String(sourceKey),
		Body:   bytes.NewReader(testImageData),
	})
	if err != nil {
		t.Fatalf("Failed to upload source image: %v", err)
	}
	t.Logf("Uploaded source image to MinIO: s3://%s/%s (%d bytes)", sourceBucket, sourceKey, len(testImageData))
}

func ensureImageIsPubliclyReachable(t *testing.T, minioEndpoint string, testImageData []byte) {
	// Verify the image is accessible via HTTP (from host)
	imageURL := fmt.Sprintf("%s/%s/%s", minioEndpoint, sourceBucket, sourceKey)
	t.Logf("Verifying image is accessible at: %s", imageURL)

	resp, err := http.Get(imageURL)
	if err != nil {
		t.Fatalf("Failed to fetch image via HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status 200 for source image, got %d. Body: %s", resp.StatusCode, string(bodyBytes))
	}

	fetchedImageData, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read fetched image: %v", err)
	}

	if len(fetchedImageData) != len(testImageData) {
		t.Fatalf("Fetched image size (%d) doesn't match uploaded size (%d)", len(fetchedImageData), len(testImageData))
	}

	t.Logf("✓ Source image is publicly accessible via HTTP (%d bytes)", len(fetchedImageData))
}

func startProxyContainer(t *testing.T, ctx context.Context, network *testcontainers.DockerNetwork, minioInternalEndpoint string) (testcontainers.Container, string) {
	// Build the Docker image from Dockerfile
	t.Log("Building Docker image from Dockerfile...")
	dockerfile := filepath.Join(".", "Dockerfile")

	proxyContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    ".",
				Dockerfile: dockerfile,
			},
			ExposedPorts: []string{"8080/tcp"},
			Networks:     []string{network.Name},
			Env: map[string]string{
				"S3_ENDPOINT":           minioInternalEndpoint,
				"AWS_ACCESS_KEY_ID":     minioAccessKey,
				"AWS_SECRET_ACCESS_KEY": minioSecretKey,
				"S3_BUCKET":             processedBucket,
				"IMGPROXY_USE_S3":       "true",
				"IMGPROXY_S3_ENDPOINT":  minioInternalEndpoint,
			},
			WaitingFor: wait.ForLog("imgproxy is ready").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("Failed to build/start proxy container: %v", err)
	}

	t.Logf("Using internal MinIO endpoint for containers: %s", minioInternalEndpoint)

	proxyHost, err := proxyContainer.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get proxy host: %v", err)
	}

	proxyPort, err := proxyContainer.MappedPort(ctx, "8080")
	if err != nil {
		t.Fatalf("Failed to get proxy port: %v", err)
	}

	proxyURL := fmt.Sprintf("http://%s:%s", proxyHost, proxyPort.Port())
	t.Logf("Proxy running at: %s", proxyURL)

	return proxyContainer, proxyURL
}

func requestImageThroughProxy(t *testing.T, requestURL string) []byte {
	resp, err := http.Get(requestURL)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status 200, got %d. Body: %s", resp.StatusCode, string(bodyBytes))
	}

	// Read response body
	processedImageData, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	return processedImageData
}

func getImageFromCacheBucket(t *testing.T, ctx context.Context, s3Client *s3.Client, actualRequestPath string) []byte {
	// Calculate expected S3 key using the ACTUAL path the server received
	expectedKey := GenerateS3Key(actualRequestPath)

	t.Logf("Expected S3 key: %s (from actual path: %s)", expectedKey, actualRequestPath)

	// Wait a bit for async upload to complete
	time.Sleep(2 * time.Second)

	// Verify the processed image was uploaded to MinIO
	headOutput, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(processedBucket),
		Key:    aws.String(expectedKey),
	})
	if err != nil {
		// List bucket contents for debugging
		t.Logf("Processed image not found at key: %s", expectedKey)
		t.Log("Listing bucket contents...")

		listOutput, listErr := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(processedBucket),
		})
		if listErr != nil {
			t.Logf("Failed to list bucket: %v", listErr)
		} else {
			if len(listOutput.Contents) == 0 {
				t.Log("Bucket is empty - no objects uploaded")
			} else {
				t.Logf("Found %d objects in bucket:", len(listOutput.Contents))
				for _, obj := range listOutput.Contents {
					t.Logf("  - %s (size: %d bytes)", *obj.Key, obj.Size)
				}
			}
		}

		t.Fatalf("Processed image not found in MinIO: %v", err)
	}

	t.Logf("✓ Processed image found in MinIO: s3://%s/%s (size: %d bytes)",
		processedBucket, expectedKey, headOutput.ContentLength)

	// Download and verify it matches what we received
	getOutput, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(processedBucket),
		Key:    aws.String(expectedKey),
	})
	if err != nil {
		t.Fatalf("Failed to download processed image: %v", err)
	}
	defer getOutput.Body.Close()

	storedImageData, err := io.ReadAll(getOutput.Body)
	if err != nil {
		t.Fatalf("Failed to read stored image: %v", err)
	}

	return storedImageData
}
