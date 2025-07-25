/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/semaphore"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/containerd/v2/version"
)

var (
	// ErrInvalidAuthorization is used when credentials are passed to a server but
	// those credentials are rejected.
	ErrInvalidAuthorization = errors.New("authorization failed")

	// MaxManifestSize represents the largest size accepted from a registry
	// during resolution. Larger manifests may be accepted using a
	// resolution method other than the registry.
	//
	// NOTE: The max supported layers by some runtimes is 128 and individual
	// layers will not contribute more than 256 bytes, making a
	// reasonable limit for a large image manifests of 32K bytes.
	// 4M bytes represents a much larger upper bound for images which may
	// contain large annotations or be non-images. A proper manifest
	// design puts large metadata in subobjects, as is consistent the
	// intent of the manifest design.
	MaxManifestSize int64 = 4 * 1048 * 1048
)

// Authorizer is used to authorize HTTP requests based on 401 HTTP responses.
// An Authorizer is responsible for caching tokens or credentials used by
// requests.
type Authorizer interface {
	// Authorize sets the appropriate `Authorization` header on the given
	// request.
	//
	// If no authorization is found for the request, the request remains
	// unmodified. It may also add an `Authorization` header as
	//  "bearer <some bearer token>"
	//  "basic <base64 encoded credentials>"
	//
	// It may return remotes/errors.ErrUnexpectedStatus, which for example,
	// can be used by the caller to find out the status code returned by the registry.
	Authorize(context.Context, *http.Request) error

	// AddResponses adds a 401 response for the authorizer to consider when
	// authorizing requests. The last response should be unauthorized and
	// the previous requests are used to consider redirects and retries
	// that may have led to the 401.
	//
	// If response is not handled, returns `ErrNotImplemented`
	AddResponses(context.Context, []*http.Response) error
}

// ResolverOptions are used to configured a new Docker register resolver
type ResolverOptions struct {
	// Hosts returns registry host configurations for a namespace.
	Hosts RegistryHosts

	// Headers are the HTTP request header fields sent by the resolver
	Headers http.Header

	// Tracker is used to track uploads to the registry. This is used
	// since the registry does not have upload tracking and the existing
	// mechanism for getting blob upload status is expensive.
	Tracker StatusTracker

	// Authorizer is used to authorize registry requests
	//
	// Deprecated: use Hosts.
	Authorizer Authorizer

	// Credentials provides username and secret given a host.
	// If username is empty but a secret is given, that secret
	// is interpreted as a long lived token.
	//
	// Deprecated: use Hosts.
	Credentials func(string) (string, string, error)

	// Host provides the hostname given a namespace.
	//
	// Deprecated: use Hosts.
	Host func(string) (string, error)

	// PlainHTTP specifies to use plain http and not https
	//
	// Deprecated: use Hosts.
	PlainHTTP bool

	// Client is the http client to used when making registry requests
	//
	// Deprecated: use Hosts.
	Client *http.Client
}

// DefaultHost is the default host function.
func DefaultHost(ns string) (string, error) {
	if ns == "docker.io" {
		return "registry-1.docker.io", nil
	}
	return ns, nil
}

type dockerResolver struct {
	hosts         RegistryHosts
	header        http.Header
	resolveHeader http.Header
	tracker       StatusTracker
	config        transfer.ImageResolverOptions
}

// NewResolver returns a new resolver to a Docker registry
func NewResolver(options ResolverOptions) remotes.Resolver {
	if options.Tracker == nil {
		options.Tracker = NewInMemoryTracker()
	}

	if options.Headers == nil {
		options.Headers = make(http.Header)
	} else {
		// make a copy of the headers to avoid race due to concurrent map write
		options.Headers = options.Headers.Clone()
	}

	resolveHeader := http.Header{}
	if _, ok := options.Headers["Accept"]; !ok {
		// set headers for all the types we support for resolution.
		resolveHeader.Set("Accept", strings.Join([]string{
			images.MediaTypeDockerSchema2Manifest,
			images.MediaTypeDockerSchema2ManifestList,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex, "*/*",
		}, ", "))
	} else {
		resolveHeader["Accept"] = options.Headers["Accept"]
		delete(options.Headers, "Accept")
	}

	if options.Hosts == nil {
		opts := []RegistryOpt{}
		if options.Host != nil {
			opts = append(opts, WithHostTranslator(options.Host))
		}

		if options.Authorizer == nil {
			options.Authorizer = NewDockerAuthorizer(
				WithAuthClient(options.Client),
				WithAuthHeader(options.Headers),
				WithAuthCreds(options.Credentials))
		}
		opts = append(opts, WithAuthorizer(options.Authorizer))

		if options.Client != nil {
			opts = append(opts, WithClient(options.Client))
		}
		if options.PlainHTTP {
			opts = append(opts, WithPlainHTTP(MatchAllHosts))
		} else {
			opts = append(opts, WithPlainHTTP(MatchLocalhost))
		}
		options.Hosts = ConfigureDefaultRegistries(opts...)
	}
	return &dockerResolver{
		hosts:         options.Hosts,
		header:        options.Headers,
		resolveHeader: resolveHeader,
		tracker:       options.Tracker,
	}
}

func getManifestMediaType(resp *http.Response) string {
	// Strip encoding data (manifests should always be ascii JSON)
	contentType := resp.Header.Get("Content-Type")
	if sp := strings.IndexByte(contentType, ';'); sp != -1 {
		contentType = contentType[0:sp]
	}

	// As of Apr 30 2019 the registry.access.redhat.com registry does not specify
	// the content type of any data but uses schema1 manifests.
	if contentType == "text/plain" {
		contentType = images.MediaTypeDockerSchema1Manifest
	}
	return contentType
}

type countingReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

var _ remotes.ResolverWithOptions = &dockerResolver{}

func (r *dockerResolver) Resolve(ctx context.Context, ref string) (string, ocispec.Descriptor, error) {
	base, err := r.resolveDockerBase(ref)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}

	if base.refspec.Object == "" {
		return "", ocispec.Descriptor{}, reference.ErrObjectRequired
	}

	var (
		paths [][]string
		dgst  = base.refspec.Digest()
		caps  = HostCapabilityPull
	)

	if dgst != "" {
		if err := dgst.Validate(); err != nil {
			// need to fail here, since we can't actually resolve the invalid
			// digest.
			return "", ocispec.Descriptor{}, err
		}

		// turns out, we have a valid digest, make a url.
		paths = append(paths, []string{"manifests", dgst.String()})

		// fallback to blobs on not found.
		paths = append(paths, []string{"blobs", dgst.String()})
	} else {
		// Add
		paths = append(paths, []string{"manifests", base.refspec.Object})
		caps |= HostCapabilityResolve
	}

	hosts := base.filterHosts(caps)
	if len(hosts) == 0 {
		return "", ocispec.Descriptor{}, fmt.Errorf("no resolve hosts: %w", errdefs.ErrNotFound)
	}

	var (
		// firstErr is the most relevant error encountered during resolution.
		// We use this to determine the error to return, making sure that the
		// error created furthest through the resolution process is returned.
		firstErr         error
		firstErrPriority int
	)

	nextHostOrFail := func(i int) string {
		if i < len(hosts)-1 {
			return "trying next host"
		}
		return "fetch failed"
	}

	for _, u := range paths {
		for i, host := range hosts {
			ctx := log.WithLogger(ctx, log.G(ctx).WithField("host", host.Host))
			base := base.withRewritesFromHost(host)
			ctx, err = ContextWithRepositoryScope(ctx, base.refspec, false)
			if err != nil {
				return "", ocispec.Descriptor{}, err
			}
			req := base.request(host, http.MethodHead, u...)
			if err := req.addNamespace(base.refspec.Hostname()); err != nil {
				return "", ocispec.Descriptor{}, err
			}

			for key, value := range r.resolveHeader {
				req.header[key] = append(req.header[key], value...)
			}

			log.G(ctx).Debug("resolving")
			resp, err := req.doWithRetries(ctx, i == len(hosts)-1)
			if err != nil {
				if errors.Is(err, ErrInvalidAuthorization) {
					err = fmt.Errorf("pull access denied, repository does not exist or may require authorization: %w", err)
				}
				if firstErrPriority < 1 {
					firstErr = err
					firstErrPriority = 1
				}
				log.G(ctx).WithError(err).Info(nextHostOrFail(i))
				continue // try another host
			}
			resp.Body.Close() // don't care about body contents.

			if resp.StatusCode > 299 {
				if resp.StatusCode == http.StatusNotFound {
					if firstErrPriority < 2 {
						firstErr = fmt.Errorf("%s: %w", ref, errdefs.ErrNotFound)
						firstErrPriority = 2
					}
					log.G(ctx).Infof("%s after status: %s", nextHostOrFail(i), resp.Status)
					continue
				}
				if resp.StatusCode > 399 {
					if firstErrPriority < 3 {
						firstErr = unexpectedResponseErr(resp)
						firstErrPriority = 3
					}
					log.G(ctx).Infof("%s after status: %s", nextHostOrFail(i), resp.Status)
					continue // try another host
				}
				return "", ocispec.Descriptor{}, unexpectedResponseErr(resp)
			}
			size := resp.ContentLength
			contentType := getManifestMediaType(resp)

			// if no digest was provided, then only a resolve
			// trusted registry was contacted, in this case use
			// the digest header (or content from GET)
			if dgst == "" {
				// this is the only point at which we trust the registry. we use the
				// content headers to assemble a descriptor for the name. when this becomes
				// more robust, we mostly get this information from a secure trust store.
				dgstHeader := digest.Digest(resp.Header.Get("Docker-Content-Digest"))

				if dgstHeader != "" && size != -1 {
					if err := dgstHeader.Validate(); err != nil {
						return "", ocispec.Descriptor{}, fmt.Errorf("%q in header not a valid digest: %w", dgstHeader, err)
					}
					dgst = dgstHeader
				}
			}
			if dgst == "" || size == -1 {
				log.G(ctx).Debug("no Docker-Content-Digest header, fetching manifest instead")

				req = base.request(host, http.MethodGet, u...)
				if err := req.addNamespace(base.refspec.Hostname()); err != nil {
					return "", ocispec.Descriptor{}, err
				}

				for key, value := range r.resolveHeader {
					req.header[key] = append(req.header[key], value...)
				}

				resp, err := req.doWithRetries(ctx, true)
				if err != nil {
					return "", ocispec.Descriptor{}, err
				}

				bodyReader := countingReader{reader: resp.Body}

				contentType = getManifestMediaType(resp)
				err = func() error {
					defer resp.Body.Close()
					if dgst != "" {
						_, err = io.Copy(io.Discard, &bodyReader)
						return err
					}

					if contentType == images.MediaTypeDockerSchema1Manifest {
						return fmt.Errorf("%w: media type %q is no longer supported since containerd v2.0, please rebuild the image as %q or %q",
							errdefs.ErrNotImplemented, images.MediaTypeDockerSchema1Manifest, images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest)
					}

					dgst, err = digest.FromReader(&bodyReader)
					return err
				}()
				if err != nil {
					return "", ocispec.Descriptor{}, err
				}
				size = bodyReader.bytesRead
			}
			// Prevent resolving to excessively large manifests
			if size > MaxManifestSize {
				if firstErrPriority < 4 {
					firstErr = fmt.Errorf("rejecting %d byte manifest for %s: %w", size, ref, errdefs.ErrNotFound)
					firstErrPriority = 4
				}
				continue
			}

			desc := ocispec.Descriptor{
				Digest:    dgst,
				MediaType: contentType,
				Size:      size,
			}

			log.G(ctx).WithField("desc.digest", desc.Digest).Debug("resolved")
			return ref, desc, nil
		}
	}

	// If above loop terminates without return or error, then no registries
	// were provided.
	if firstErr == nil {
		firstErr = fmt.Errorf("%s: %w", ref, errdefs.ErrNotFound)
	}

	return "", ocispec.Descriptor{}, firstErr
}

func (r *dockerBase) withRewritesFromHost(host RegistryHost) *dockerBase {
	for pattern, replace := range host.Rewrites {
		exp, err := regexp.Compile(pattern)
		if err != nil {
			log.L.WithError(err).Warnf("Failed to compile rewrite, `%s`, for %s", pattern, host.Host)
			continue
		}
		if rr := exp.ReplaceAllString(r.repository, replace); rr != r.repository {
			log.L.Debugf("Rewrote repository for %s: %s => %s", r.refspec, r.repository, rr)
			return &dockerBase{
				refspec: reference.Spec{
					Locator: r.refspec.Hostname() + "/" + rr,
					Object:  r.refspec.Object,
				},
				repository: rr,
				header:     r.header,
			}
		}
	}
	return r
}

func (r *dockerResolver) SetOptions(options ...transfer.ImageResolverOption) {
	for _, opt := range options {
		opt(&r.config)
	}
}

func (r *dockerResolver) Fetcher(ctx context.Context, ref string) (remotes.Fetcher, error) {
	base, err := r.resolveDockerBase(ref)
	if err != nil {
		return nil, err
	}

	return dockerFetcher{
		dockerBase: base,
	}, nil
}

func (r *dockerResolver) Pusher(ctx context.Context, ref string) (remotes.Pusher, error) {
	base, err := r.resolveDockerBase(ref)
	if err != nil {
		return nil, err
	}

	return dockerPusher{
		dockerBase: base,
		object:     base.refspec.Object,
		tracker:    r.tracker,
	}, nil
}

func (r *dockerResolver) resolveDockerBase(ref string) (*dockerBase, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return nil, err
	}

	return r.base(refspec)
}

type dockerBase struct {
	refspec      reference.Spec
	repository   string
	hosts        []RegistryHost
	header       http.Header
	performances transfer.ImageResolverPerformanceSettings
	limiter      *semaphore.Weighted
}

func (r *dockerBase) Acquire(ctx context.Context, weight int64) error {
	if r.limiter == nil {
		return nil
	}
	return r.limiter.Acquire(ctx, weight)
}

func (r *dockerBase) Release(weight int64) {
	if r.limiter != nil {
		r.limiter.Release(weight)
	}
}

func (r *dockerResolver) base(refspec reference.Spec) (*dockerBase, error) {
	host := refspec.Hostname()
	hosts, err := r.hosts(host)
	if err != nil {
		return nil, err
	}
	return &dockerBase{
		refspec:      refspec,
		repository:   strings.TrimPrefix(refspec.Locator, host+"/"),
		hosts:        hosts,
		header:       r.header,
		performances: r.config.Performances,
		limiter:      r.config.DownloadLimiter,
	}, nil
}

func (r *dockerBase) filterHosts(caps HostCapabilities) (hosts []RegistryHost) {
	for _, host := range r.hosts {
		if host.Capabilities.Has(caps) {
			hosts = append(hosts, host)
		}
	}
	return
}

func (r *dockerBase) request(host RegistryHost, method string, ps ...string) *request {
	header := r.header.Clone()
	if header == nil {
		header = http.Header{}
	}

	for key, value := range host.Header {
		header[key] = append(header[key], value...)
	}

	if len(header.Get("User-Agent")) == 0 {
		header.Set("User-Agent", "containerd/"+version.Version)
	}

	parts := append([]string{"/", host.Path, r.repository}, ps...)
	p := path.Join(parts...)
	// Join strips trailing slash, re-add ending "/" if included
	if len(parts) > 0 && strings.HasSuffix(parts[len(parts)-1], "/") {
		p = p + "/"
	}
	return &request{
		method: method,
		path:   p,
		header: header,
		host:   host,
	}
}

func (r *request) authorize(ctx context.Context, req *http.Request) error {
	// Check if has header for host
	if r.host.Authorizer != nil {
		if err := r.host.Authorizer.Authorize(ctx, req); err != nil {
			return err
		}
	}

	return nil
}

func (r *request) addNamespace(ns string) (err error) {
	if !r.host.isProxy(ns) {
		return nil
	}
	var q url.Values
	// Parse query
	if i := strings.IndexByte(r.path, '?'); i > 0 {
		r.path = r.path[:i+1]
		q, err = url.ParseQuery(r.path[i+1:])
		if err != nil {
			return
		}
	} else {
		r.path = r.path + "?"
		q = url.Values{}
	}
	q.Add("ns", ns)

	r.path = r.path + q.Encode()

	return
}

type request struct {
	method string
	path   string
	header http.Header
	host   RegistryHost
	body   func() (io.ReadCloser, error)
	size   int64
}

func (r *request) clone() *request {
	res := *r
	res.header = r.header.Clone()
	return &res
}

func (r *request) do(ctx context.Context) (*http.Response, error) {
	u := r.host.Scheme + "://" + r.host.Host + r.path
	req, err := http.NewRequestWithContext(ctx, r.method, u, nil)
	if err != nil {
		return nil, err
	}
	if r.header == nil {
		req.Header = http.Header{}
	} else {
		req.Header = r.header.Clone() // headers need to be copied to avoid concurrent map access
	}
	if r.body != nil {
		body, err := r.body()
		if err != nil {
			return nil, err
		}
		req.Body = body
		req.GetBody = r.body
		if r.size > 0 {
			req.ContentLength = r.size
		}
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("url", u))
	log.G(ctx).WithFields(requestFields(req)).Debug("do request")
	if err := r.authorize(ctx, req); err != nil {
		return nil, fmt.Errorf("failed to authorize: %w", err)
	}

	client := &http.Client{}
	if r.host.Client != nil {
		*client = *r.host.Client
	}
	if client.CheckRedirect == nil {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if err := r.authorize(ctx, req); err != nil {
				return fmt.Errorf("failed to authorize redirect: %w", err)
			}
			return nil
		}
	}

	tracing.UpdateHTTPClient(client, tracing.Name("remotes.docker.resolver", "HTTPRequest"))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do request: %w", err)
	}
	log.G(ctx).WithFields(responseFields(resp)).Debug("fetch response received")
	return resp, nil
}

type doChecks func(r *request, resp *http.Response) error

func withErrorCheck(r *request, resp *http.Response) error {
	if resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("content at %v not found: %w", r.String(), errdefs.ErrNotFound)
		}

		return unexpectedResponseErr(resp)
	}
	return nil
}

var errContentRangeIgnored = errors.New("content range requests ignored")

func withOffsetCheck(offset int64) doChecks {
	return func(r *request, resp *http.Response) error {
		if offset == 0 {
			return nil
		}
		if resp.StatusCode == http.StatusPartialContent {
			return nil
		}
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
				return fmt.Errorf("unhandled content range in response: %v", cr)
			}
			return nil
		}

		// Discard up to offset
		// Could use buffer pool here but this case should be rare
		n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, offset))
		if err != nil {
			return fmt.Errorf("failed to discard to offset: %w", err)
		}
		if n != offset {
			return errors.New("unable to discard to offset")
		}

		// content range ignored, we can't do concurrent fetches here.
		// return an error to be caught
		return errContentRangeIgnored
	}
}

func (r *request) doWithRetries(ctx context.Context, lastHost bool, checks ...doChecks) (resp *http.Response, err error) {
	resp, err = r.doWithRetriesInner(ctx, nil, lastHost)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && err != errContentRangeIgnored {
			resp.Body.Close()
		}
	}()
	for _, check := range checks {
		if err := check(r, resp); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

func (r *request) doWithRetriesInner(ctx context.Context, responses []*http.Response, lastHost bool) (*http.Response, error) {
	resp, err := r.do(ctx)
	if err != nil {
		return nil, err
	}

	responses = append(responses, resp)
	retry, err := r.retryRequest(ctx, responses, lastHost)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if retry {
		resp.Body.Close()
		return r.doWithRetriesInner(ctx, responses, lastHost)
	}
	return resp, err
}

func (r *request) retryRequest(ctx context.Context, responses []*http.Response, lastHost bool) (bool, error) {
	if len(responses) > 5 {
		return false, nil
	}
	last := responses[len(responses)-1]
	switch last.StatusCode {
	case http.StatusUnauthorized:
		log.G(ctx).WithField("header", last.Header.Get("WWW-Authenticate")).Debug("Unauthorized")
		if r.host.Authorizer != nil {
			if err := r.host.Authorizer.AddResponses(ctx, responses); err == nil {
				return true, nil
			} else if !errdefs.IsNotImplemented(err) {
				return false, err
			}
		}

		return false, nil
	case http.StatusMethodNotAllowed:
		// Support registries which have not properly implemented the HEAD method for
		// manifests endpoint
		if r.method == http.MethodHead && strings.Contains(r.path, "/manifests/") {
			r.method = http.MethodGet
			return true, nil
		}
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true, nil
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError:
		// Do not retry if the same error was seen in the last request
		if len(responses) > 1 && responses[len(responses)-2].StatusCode == last.StatusCode {
			return false, nil
		}
		// Only retry if this is the last host that will be attempted
		if lastHost {
			return true, nil
		}
	}

	return false, nil
}

func (r *request) String() string {
	return r.host.Scheme + "://" + r.host.Host + r.path
}

func (r *request) setMediaType(mediatype string) {
	if mediatype == "" {
		r.header.Set("Accept", "*/*")
	} else {
		r.header.Set("Accept", strings.Join([]string{mediatype, `*/*`}, ", "))
	}
}

func (r *request) setOffset(offset int64) {
	r.header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
}

func requestFields(req *http.Request) log.Fields {
	fields := map[string]interface{}{
		"request.method": req.Method,
	}
	for k, vals := range req.Header {
		k = strings.ToLower(k)
		if k == "authorization" {
			continue
		}
		for i, v := range vals {
			field := "request.header." + k
			if i > 0 {
				field = fmt.Sprintf("%s.%d", field, i)
			}
			fields[field] = v
		}
	}

	return fields
}

func responseFields(resp *http.Response) log.Fields {
	fields := map[string]interface{}{
		"response.status": resp.Status,
	}
	for k, vals := range resp.Header {
		k = strings.ToLower(k)
		for i, v := range vals {
			field := "response.header." + k
			if i > 0 {
				field = fmt.Sprintf("%s.%d", field, i)
			}
			fields[field] = v
		}
	}

	return fields
}

// IsLocalhost checks if the registry host is local.
func IsLocalhost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip.IsLoopback()
}

// NewHTTPFallback returns http.RoundTripper which allows fallback from https to
// http for registry endpoints with configurations for both http and TLS,
// such as defaulted localhost endpoints.
func NewHTTPFallback(transport http.RoundTripper) http.RoundTripper {
	return &httpFallback{
		super: transport,
	}
}

type httpFallback struct {
	super http.RoundTripper
	host  string
	mu    sync.Mutex
}

func (f *httpFallback) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	fallback := f.host == r.URL.Host
	f.mu.Unlock()

	// only fall back if the same host had previously fell back
	if !fallback {
		resp, err := f.super.RoundTrip(r)
		if !isTLSError(err) && !isPortError(err, r.URL.Host) {
			return resp, err
		}
	}

	plainHTTPUrl := *r.URL
	plainHTTPUrl.Scheme = "http"

	plainHTTPRequest := *r
	plainHTTPRequest.URL = &plainHTTPUrl

	if !fallback {
		f.mu.Lock()
		if f.host != r.URL.Host {
			f.host = r.URL.Host
		}
		f.mu.Unlock()

		// update body on the second attempt
		if r.Body != nil && r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			plainHTTPRequest.Body = body
		}
	}

	return f.super.RoundTrip(&plainHTTPRequest)
}

func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(err, &tlsErr) && string(tlsErr.RecordHeader[:]) == "HTTP/" {
		return true
	}
	if strings.Contains(err.Error(), "TLS handshake timeout") {
		return true
	}

	return false
}

func isPortError(err error, host string) bool {
	if isConnError(err) || os.IsTimeout(err) {
		if _, port, _ := net.SplitHostPort(host); port != "" {
			// Port is specified, will not retry on different port with scheme change
			return false
		}
		return true
	}

	return false
}
