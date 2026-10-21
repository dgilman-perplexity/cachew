package strategy //nolint:testpackage // Exercise the SDK transport and cache without AWS.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alecthomas/assert/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awscodeartifact "github.com/aws/aws-sdk-go-v2/service/codeartifact"

	"github.com/block/cachew/internal/cache"
	"github.com/block/cachew/internal/logging"
)

func TestCodeArtifactAssetAPI(t *testing.T) {
	calls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v1/package/version/asset", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "example", r.URL.Query().Get("domain"))
		assert.Equal(t, "123456789012", r.URL.Query().Get("domain-owner"))
		assert.Equal(t, "packages", r.URL.Query().Get("repository"))
		assert.Equal(t, "org.example", r.URL.Query().Get("namespace"))
		assert.Equal(t, "maven", r.URL.Query().Get("format"))
		assert.Equal(t, "tool", r.URL.Query().Get("package"))
		assert.Equal(t, "1", r.URL.Query().Get("version"))
		assert.Contains(t, r.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
		assert.NotEqual(t, "Bearer caller", r.Header.Get("Authorization"))
		switch r.URL.Query().Get("asset") {
		case "missing.tar":
			w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"missing"}`))
		case "denied.tar":
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"private origin detail"}`))
		default:
			w.Header().Set("X-Packageversionrevision", "revision-1")
			w.Header().Set("Content-Length", "7")
			w.Header().Set("X-Secret", "not-for-client")
			_, _ = w.Write([]byte("archive"))
		}
	}))
	defer origin.Close()
	_, ctx := logging.Configure(context.Background(), logging.Config{Level: slog.LevelError})
	memory, err := cache.NewMemory(ctx, cache.MemoryConfig{LimitMB: 1, MaxTTL: time.Hour})
	assert.NoError(t, err)
	defer memory.Close()
	config := testCodeArtifactConfig(origin.URL)
	config.AssetAPI = true
	config.AliasesOnly = true
	config.ImmutableFallbackTTL = time.Hour
	config.Aliases = []CodeArtifactAliasConfig{{Pattern: "/downloads/{version}/{asset}", Path: "/maven/packages/org/example/tool/{version}/{asset}"}}
	mux := http.NewServeMux()
	strategy, err := newCodeArtifact(ctx, config, mux, nil, memory, true)
	assert.NoError(t, err)
	strategy.assets = &codeArtifactAssetOrigin{
		domain: "example", owner: "123456789012",
		client: awscodeartifact.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""),
		}, func(o *awscodeartifact.Options) { o.BaseEndpoint = aws.String(origin.URL); o.RetryMaxAttempts = 1 }),
	}
	request := func(method, asset string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/downloads/1/"+asset, nil)
		r.Header.Set("Authorization", "Bearer caller")
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	first := request("GET", "tool.tar", nil)
	assert.Equal(t, 200, first.Code)
	assert.Equal(t, "archive", first.Body.String())
	assert.Equal(t, "7", first.Header().Get("Content-Length"))
	assert.Equal(t, "", first.Header().Get("X-Secret"))
	assert.Equal(t, "archive", request("GET", "tool.tar", nil).Body.String())
	assert.Equal(t, 1, calls)
	etag := first.Header().Get("ETag")
	assert.Equal(t, 304, request("GET", "tool.tar", map[string]string{"If-None-Match": etag}).Code)
	assert.Equal(t, 412, request("GET", "tool.tar", map[string]string{"If-Match": `"other"`}).Code)
	assert.Equal(t, 1, calls)
	for asset, status := range map[string]int{"missing.tar": http.StatusNotFound, "denied.tar": http.StatusForbidden} {
		for range 2 {
			response := request("GET", asset, nil)
			assert.Equal(t, status, response.Code)
			assert.Equal(t, "CodeArtifact asset unavailable\n", response.Body.String())
		}
	}
	assert.Equal(t, 5, calls)
	head := request("HEAD", "cold.tar", nil)
	assert.Equal(t, 200, head.Code)
	assert.Equal(t, "", head.Body.String())
	assert.Equal(t, "7", head.Header().Get("Content-Length"))
	assert.Equal(t, 304, request("GET", "cold.tar", map[string]string{"If-None-Match": etag}).Code)
	ranged := request("GET", "cold.tar", map[string]string{"Range": "bytes=0-1"})
	assert.Equal(t, 200, ranged.Code)
	assert.Equal(t, "archive", ranged.Body.String())
	assert.Equal(t, 8, calls)
	assert.Equal(t, "archive", request("GET", "cold.tar", nil).Body.String())
	assert.Equal(t, 9, calls)
	snapshot := httptest.NewRecorder()
	mux.ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, "/downloads/1-SNAPSHOT/tool.tar", nil))
	assert.Equal(t, 404, snapshot.Code)
	assert.Equal(t, 9, calls)
}
