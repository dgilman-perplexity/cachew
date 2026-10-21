package strategy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alecthomas/errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awscodeartifact "github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type codeArtifactAssetOrigin struct {
	client *awscodeartifact.Client
	domain string
	owner  string
}

func (c *CodeArtifact) serveAsset(w http.ResponseWriter, r *http.Request, mode codeArtifactCacheMode) {
	origin := c.originURL(r)
	parts := strings.Split(strings.TrimPrefix(origin.Path, "/"), "/")
	if len(parts) < 6 || parts[0] != "maven" || origin.RawQuery != "" || origin.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	for i, part := range parts {
		if !aliasSegment.MatchString(part) || part == "." || part == ".." || (i == len(parts)-2 && strings.HasSuffix(part, "-SNAPSHOT")) {
			http.NotFound(w, r)
			return
		}
	}
	n := len(parts)
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	started := time.Now()
	output, err := c.assets.client.GetPackageVersionAsset(ctx, &awscodeartifact.GetPackageVersionAssetInput{
		Domain: aws.String(c.assets.domain), DomainOwner: aws.String(c.assets.owner),
		Repository: aws.String(parts[1]), Format: types.PackageFormatMaven,
		Namespace: aws.String(strings.Join(parts[2:n-3], ".")), Package: aws.String(parts[n-3]),
		PackageVersion: aws.String(parts[n-2]), Asset: aws.String(parts[n-1]),
	})
	if err != nil {
		var responseError *smithyhttp.ResponseError
		if errors.As(err, &responseError) {
			switch responseError.HTTPStatusCode() {
			case http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests:
				http.Error(w, "CodeArtifact asset unavailable", responseError.HTTPStatusCode())
				return
			}
		}
		c.writeError(w, r, errors.New("request CodeArtifact asset"))
		return
	}
	body := newCodeArtifactOriginBody(ctx, output.Asset, cancel, c.originReadIdleTimeout)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set("Cache-Control", "public, immutable")
	if raw, ok := awsmiddleware.GetRawResponse(output.ResultMetadata).(*smithyhttp.Response); ok {
		if length := raw.Header.Get("Content-Length"); length != "" {
			headers.Set("Content-Length", length)
		}
	}
	if revision := aws.ToString(output.PackageVersionRevision); revision != "" {
		headers.Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(revision))))
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers, Body: body}
	c.observeOriginResponse(r.Context(), resp, origin.Path, started)
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.ErrorContext(r.Context(), "Failed to close CodeArtifact asset", "error", err)
		}
	}()
	if status := cachedPreconditionStatus(r, headers); status != 0 {
		if status == http.StatusNotModified {
			copyHeaders(w.Header(), headers)
		}
		w.WriteHeader(status)
		return
	}
	copyHeaders(w.Header(), headers)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	// The asset API has no range operation. HTTP permits a complete 200 response
	// to Range; these reads stay uncached rather than storing a partial object.
	if mode == codeArtifactCacheLookup {
		c.streamAndCache(w, r, resp, headers)
	} else {
		c.streamOriginBody(r.Context(), w, resp.Body)
	}
}
