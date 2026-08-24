package s3

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
)

func TestStoreSignedStreamingContract(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=access/") || request.Header.Get("X-Amz-Date") == "" {
			http.Error(response, "unsigned", http.StatusForbidden)
			return
		}
		if request.URL.Path == "/artifacts" {
			response.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch request.Method {
		case http.MethodPut:
			if _, exists := objects[request.URL.Path]; exists && request.Header.Get("If-None-Match") == "*" {
				response.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			data, _ := io.ReadAll(request.Body)
			objects[request.URL.Path] = data
			response.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, exists := objects[request.URL.Path]
			if !exists {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = response.Write(data)
		case http.MethodDelete:
			delete(objects, request.URL.Path)
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "artifacts", AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	if err := store.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	options := artifact.PutOptions{MediaType: "text/plain", EncryptionMode: artifact.EncryptionClient, CreateOnly: true}
	info, err := store.Put(ctx, "tenants/ten_0001/artifacts/art_0001", strings.NewReader("payload"), 7, options)
	if err != nil || info.Size != 7 {
		t.Fatalf("Put = %+v/%v", info, err)
	}
	if _, err := store.Put(ctx, "tenants/ten_0001/artifacts/art_0001", strings.NewReader("payload"), 7, options); err != artifact.ErrAlreadyExists {
		t.Fatalf("duplicate error = %v", err)
	}
	reader, err := store.Open(ctx, "tenants/ten_0001/artifacts/art_0001")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(data, []byte("payload")) {
		t.Fatalf("Open = %q/%v", data, err)
	}
	if err := store.Delete(ctx, "tenants/ten_0001/artifacts/art_0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, "tenants/ten_0001/artifacts/art_0001"); err != artifact.ErrNotFound {
		t.Fatalf("missing error = %v", err)
	}
}

func TestStoreValidation(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{{}, {Endpoint: "ftp://host", Region: "x", Bucket: "bucket", AccessKey: "a", SecretKey: "s"}} {
		if _, err := New(config); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	store, _ := New(Config{Endpoint: "http://localhost:9000", Region: "us-east-1", Bucket: "artifacts", AccessKey: "a", SecretKey: "s"})
	if _, err := store.Put(context.Background(), "../escape", strings.NewReader("x"), 1, artifact.PutOptions{}); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestStoreMultipartUploadStreamsAndCompletes(t *testing.T) {
	t.Parallel()
	var partSizes []int
	var completion string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.RawQuery == "uploads=":
			response.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(response, `<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("uploadId") == "upload-1":
			data, _ := io.ReadAll(request.Body)
			partSizes = append(partSizes, len(data))
			response.Header().Set("ETag", `"part-`+request.URL.Query().Get("partNumber")+`"`)
			response.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "upload-1":
			data, _ := io.ReadAll(request.Body)
			completion = string(data)
			response.WriteHeader(http.StatusOK)
		default:
			http.Error(response, "unexpected multipart request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	store, _ := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "artifacts", AccessKey: "access", SecretKey: "secret"})
	store.multipartThreshold = 4
	store.partSize = 5
	payload := []byte("twelve-bytes")
	info, err := store.Put(context.Background(), "tenants/ten_0001/artifacts/art_0001", bytes.NewReader(payload), int64(len(payload)),
		artifact.PutOptions{MediaType: "application/octet-stream", EncryptionMode: artifact.EncryptionClient, CreateOnly: true})
	if err != nil || info.Size != int64(len(payload)) {
		t.Fatalf("multipart Put = %+v/%v", info, err)
	}
	if len(partSizes) != 3 || partSizes[0] != 5 || partSizes[1] != 5 || partSizes[2] != 2 {
		t.Fatalf("part sizes = %v", partSizes)
	}
	if strings.Count(completion, "<Part>") != 3 || !strings.Contains(completion, "&#34;part-3&#34;") {
		t.Fatalf("completion XML = %s", completion)
	}
}

func TestStoreMultipartFailureAborts(t *testing.T) {
	t.Parallel()
	aborted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			_, _ = io.WriteString(response, `<InitiateMultipartUploadResult><UploadId>upload-2</UploadId></InitiateMultipartUploadResult>`)
		case http.MethodPut:
			http.Error(response, "part failed", http.StatusServiceUnavailable)
		case http.MethodDelete:
			aborted = request.URL.Query().Get("uploadId") == "upload-2"
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	store, _ := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "artifacts", AccessKey: "access", SecretKey: "secret"})
	store.multipartThreshold = 1
	store.partSize = 2
	_, err := store.Put(context.Background(), "tenants/ten_0001/artifacts/art_0001", strings.NewReader("failure"), 7,
		artifact.PutOptions{MediaType: "text/plain", EncryptionMode: artifact.EncryptionSSES3, CreateOnly: true})
	if err == nil || !aborted {
		t.Fatalf("multipart failure=%v aborted=%v", err, aborted)
	}
}
