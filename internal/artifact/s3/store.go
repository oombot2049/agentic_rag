// Package s3 implements the Artifact object-store contract with AWS Signature V4.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
)

const unsignedPayload = "UNSIGNED-PAYLOAD"

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Client    *http.Client
}

type Store struct {
	endpoint           *url.URL
	region             string
	bucket             string
	access             string
	secret             string
	client             *http.Client
	now                func() time.Time
	multipartThreshold int64
	partSize           int64
}

func New(config Config) (*Store, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.Path != "" {
		return nil, fmt.Errorf("%w: invalid S3 endpoint", artifact.ErrInvalidArgument)
	}
	if !validName(config.Region, 1, 63) || !bucketPattern.MatchString(config.Bucket) || strings.Contains(config.Bucket, "..") || strings.TrimSpace(config.AccessKey) == "" || strings.TrimSpace(config.SecretKey) == "" {
		return nil, fmt.Errorf("%w: invalid S3 region, bucket or credentials", artifact.ErrInvalidArgument)
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &Store{endpoint: endpoint, region: config.Region, bucket: config.Bucket, access: config.AccessKey,
		secret: config.SecretKey, client: client, now: time.Now, multipartThreshold: 5 << 30, partSize: 64 << 20}, nil
}

func (s *Store) Put(ctx context.Context, key string, source io.Reader, size int64, options artifact.PutOptions) (artifact.ObjectInfo, error) {
	if err := validateKey(key); err != nil || source == nil || size < 0 {
		return artifact.ObjectInfo{}, fmt.Errorf("%w: invalid S3 put", artifact.ErrInvalidArgument)
	}
	if size > s.multipartThreshold {
		return s.putMultipart(ctx, key, source, size, options)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", options.MediaType)
	if options.CreateOnly {
		headers.Set("If-None-Match", "*")
	}
	if err := applyEncryption(headers, options); err != nil {
		return artifact.ObjectInfo{}, err
	}
	response, err := s.do(ctx, http.MethodPut, s.objectPath(key), source, size, headers, unsignedPayload)
	if err != nil {
		return artifact.ObjectInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusConflict {
		return artifact.ObjectInfo{}, artifact.ErrAlreadyExists
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return artifact.ObjectInfo{}, responseError(response)
	}
	return artifact.ObjectInfo{Size: size}, nil
}

type initiateResult struct {
	UploadID string `xml:"UploadId"`
}
type completedPart struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
}
type completeRequest struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

func (s *Store) putMultipart(ctx context.Context, key string, source io.Reader, size int64, options artifact.PutOptions) (artifact.ObjectInfo, error) {
	headers := make(http.Header)
	headers.Set("Content-Type", options.MediaType)
	if err := applyEncryption(headers, options); err != nil {
		return artifact.ObjectInfo{}, err
	}
	response, err := s.doQuery(ctx, http.MethodPost, s.objectPath(key), "uploads=", nil, 0, headers, emptyHash())
	if err != nil {
		return artifact.ObjectInfo{}, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return artifact.ObjectInfo{}, responseError(response)
	}
	var initiated initiateResult
	err = xml.NewDecoder(response.Body).Decode(&initiated)
	_ = response.Body.Close()
	if err != nil {
		return artifact.ObjectInfo{}, fmt.Errorf("decode S3 multipart initiation: %w", err)
	}
	if initiated.UploadID == "" {
		return artifact.ObjectInfo{}, errors.New("S3 multipart initiation omitted upload ID")
	}
	abort := func(cause error) (artifact.ObjectInfo, error) {
		abortResponse, abortErr := s.doQuery(ctx, http.MethodDelete, s.objectPath(key), "uploadId="+url.QueryEscape(initiated.UploadID), nil, 0, nil, emptyHash())
		if abortResponse != nil {
			_ = abortResponse.Body.Close()
		}
		return artifact.ObjectInfo{}, errors.Join(cause, abortErr)
	}
	partSize := max(s.partSize, (size+9999)/10000)
	if partSize > 5<<30 {
		return abort(fmt.Errorf("%w: object exceeds S3 multipart limits", artifact.ErrInvalidArgument))
	}
	parts := make([]completedPart, 0, (size+partSize-1)/partSize)
	remaining := size
	for number := 1; remaining > 0; number++ {
		length := min(remaining, partSize)
		query := fmt.Sprintf("partNumber=%d&uploadId=%s", number, url.QueryEscape(initiated.UploadID))
		partResponse, err := s.doQuery(ctx, http.MethodPut, s.objectPath(key), query, io.LimitReader(source, length), length, nil, unsignedPayload)
		if err != nil {
			return abort(err)
		}
		if partResponse.StatusCode != http.StatusOK {
			failure := responseError(partResponse)
			_ = partResponse.Body.Close()
			return abort(failure)
		}
		etag := partResponse.Header.Get("ETag")
		_ = partResponse.Body.Close()
		if etag == "" {
			return abort(errors.New("S3 multipart response omitted ETag"))
		}
		parts = append(parts, completedPart{Number: number, ETag: etag})
		remaining -= length
	}
	completion, err := xml.Marshal(completeRequest{Parts: parts})
	if err != nil {
		return abort(err)
	}
	completionHash := sha256.Sum256(completion)
	completeHeaders := make(http.Header)
	completeHeaders.Set("Content-Type", "application/xml")
	if options.CreateOnly {
		completeHeaders.Set("If-None-Match", "*")
	}
	completeResponse, err := s.doQuery(ctx, http.MethodPost, s.objectPath(key), "uploadId="+url.QueryEscape(initiated.UploadID),
		bytes.NewReader(completion), int64(len(completion)), completeHeaders, hex.EncodeToString(completionHash[:]))
	if err != nil {
		return abort(err)
	}
	defer completeResponse.Body.Close()
	if completeResponse.StatusCode == http.StatusPreconditionFailed || completeResponse.StatusCode == http.StatusConflict {
		return abort(artifact.ErrAlreadyExists)
	}
	if completeResponse.StatusCode != http.StatusOK {
		return abort(responseError(completeResponse))
	}
	completeBody, err := io.ReadAll(io.LimitReader(completeResponse.Body, 1<<20))
	if err != nil {
		return abort(fmt.Errorf("read S3 multipart completion: %w", err))
	}
	if bytes.Contains(completeBody, []byte("<Error>")) {
		return abort(fmt.Errorf("S3 multipart completion error: %s", strings.TrimSpace(string(completeBody))))
	}
	return artifact.ObjectInfo{Size: size}, nil
}

func applyEncryption(headers http.Header, options artifact.PutOptions) error {
	switch options.EncryptionMode {
	case artifact.EncryptionSSES3:
		headers.Set("X-Amz-Server-Side-Encryption", "AES256")
	case artifact.EncryptionSSEKMS:
		headers.Set("X-Amz-Server-Side-Encryption", "aws:kms")
		headers.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", options.EncryptionKey)
	case artifact.EncryptionClient:
	default:
		return fmt.Errorf("%w: unsupported encryption mode", artifact.ErrInvalidArgument)
	}
	return nil
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	response, err := s.do(ctx, http.MethodGet, s.objectPath(key), nil, 0, nil, emptyHash())
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return nil, artifact.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, responseError(response)
	}
	return response.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	response, err := s.do(ctx, http.MethodDelete, s.objectPath(key), nil, 0, nil, emptyHash())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK || response.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(response)
}

// CreateBucket creates the configured private bucket for bootstrap and tests.
func (s *Store) CreateBucket(ctx context.Context) error {
	response, err := s.do(ctx, http.MethodPut, "/"+s.bucket, nil, 0, nil, emptyHash())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated || response.StatusCode == http.StatusConflict {
		return nil
	}
	return responseError(response)
}

func (s *Store) do(ctx context.Context, method, path string, body io.Reader, size int64, headers http.Header, payloadHash string) (*http.Response, error) {
	return s.doQuery(ctx, method, path, "", body, size, headers, payloadHash)
}

func (s *Store) doQuery(ctx context.Context, method, path, rawQuery string, body io.Reader, size int64, headers http.Header, payloadHash string) (*http.Response, error) {
	requestURL := *s.endpoint
	requestURL.Path = path
	requestURL.RawQuery = rawQuery
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create S3 request: %w", err)
	}
	if size > 0 || method == http.MethodPut {
		request.ContentLength = size
	}
	for name, values := range headers {
		request.Header[name] = append([]string(nil), values...)
	}
	s.sign(request, payloadHash, s.now().UTC())
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute S3 request: %w", err)
	}
	return response, nil
}

func (s *Store) sign(request *http.Request, payloadHash string, at time.Time) {
	date := at.Format("20060102")
	amzTime := at.Format("20060102T150405Z")
	request.Header.Set("X-Amz-Date", amzTime)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	canonicalHeaders, signedHeaders := canonicalHeaders(request)
	canonicalRequest := strings.Join([]string{request.Method, request.URL.EscapedPath(), request.URL.RawQuery,
		canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := date + "/" + s.region + "/s3/aws4_request"
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzTime + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])
	dateKey := hmacSum([]byte("AWS4"+s.secret), date)
	regionKey := hmacSum(dateKey, s.region)
	serviceKey := hmacSum(regionKey, "s3")
	signingKey := hmacSum(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSum(signingKey, stringToSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.access+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalHeaders(request *http.Request) (string, string) {
	values := map[string]string{"host": request.URL.Host}
	for name, entries := range request.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "content-length" || lower == "user-agent" {
			continue
		}
		parts := make([]string, len(entries))
		for index, entry := range entries {
			parts[index] = strings.Join(strings.Fields(entry), " ")
		}
		values[lower] = strings.Join(parts, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + values[name] + "\n")
	}
	return canonical.String(), strings.Join(names, ";")
}

func (s *Store) objectPath(key string) string { return "/" + s.bucket + "/" + key }

func hmacSum(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}

func emptyHash() string {
	hash := sha256.Sum256(nil)
	return hex.EncodeToString(hash[:])
}

func validateKey(key string) error {
	if key == "" || len(key) > 1024 || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.ContainsAny(key, "\\\r\n\t ?#%") {
		return fmt.Errorf("%w: invalid object key", artifact.ErrInvalidArgument)
	}
	return nil
}

func validName(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/\\\r\n ")
}

func responseError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("S3 response %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
}

var _ artifact.ObjectStore = (*Store)(nil)
