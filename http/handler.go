// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/http/pprof"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-secure-stdlib/parseutil"
	"github.com/hashicorp/go-sockaddr"
	"github.com/hashicorp/go-uuid"
	gziphandler "github.com/klauspost/compress/gzhttp"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/internalshared/configutil"
	"github.com/openbao/openbao/internalshared/listenerutil"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/helper/jsonutil"
	"github.com/openbao/openbao/sdk/v2/helper/pathmanager"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/vault"
)

const (
	// WrapTTLHeaderName is the name of the header containing a directive to
	// wrap the response
	WrapTTLHeaderName = "X-Vault-Wrap-TTL"

	// WrapFormatHeaderName is the name of the header containing the format to
	// wrap in; has no effect if the wrap TTL is not set
	WrapFormatHeaderName = "X-Vault-Wrap-Format"

	// NoRequestForwardingHeaderName is the name of the header telling Vault
	// not to use request forwarding
	NoRequestForwardingHeaderName = "X-Vault-No-Request-Forwarding"

	// MFAHeaderName represents the HTTP header which carries the credentials
	// required to perform MFA on any path.
	MFAHeaderName = "X-Vault-MFA"

	// canonicalMFAHeaderName is the MFA header value's format in the request
	// headers. Do not alter the casing of this string.
	canonicalMFAHeaderName = "X-Vault-Mfa"

	// PolicyOverrideHeaderName is the header set to request overriding
	// soft-mandatory Sentinel policies.
	PolicyOverrideHeaderName = "X-Vault-Policy-Override"

	// DefaultMaxRequestSize is the default maximum accepted request size. This
	// is to prevent a denial of service attack where no Content-Length is
	// provided and the server is fed ever more data until it exhausts memory.
	// Can be overridden per listener.
	DefaultMaxRequestSize = 32 * 1024 * 1024
)

var (
	// Set to false by stub_asset if the ui build tag isn't enabled
	uiBuiltIn = true

	alwaysRedirectPaths = pathmanager.New()

	// restrictedSysAPIsNonLogical is the set of `sys/` APIs available only in the root namespace
	// that may not pass through logical request handling but are handled in HTTP using specialized
	// calls to core. This is a subset of `vault.restrictedSysAPIs`.
	restrictedSysAPIsNonLogical = pathmanager.New()

	injectDataIntoTopRoutes = []string{
		"/v1/sys/audit",
		"/v1/sys/audit/",
		"/v1/sys/audit-hash/",
		"/v1/sys/auth",
		"/v1/sys/auth/",
		"/v1/sys/config/cors",
		"/v1/sys/config/auditing/request-headers/",
		"/v1/sys/config/auditing/request-headers",
		"/v1/sys/capabilities",
		"/v1/sys/capabilities-accessor",
		"/v1/sys/capabilities-self",
		"/v1/sys/ha-status",
		"/v1/sys/key-status",
		"/v1/sys/mounts",
		"/v1/sys/mounts/",
		"/v1/sys/policy",
		"/v1/sys/policy/",
		"/v1/sys/rekey/backup",
		"/v1/sys/rekey/recovery-key-backup",
		"/v1/sys/rotate/root/backup",
		"/v1/sys/rotate/recovery/backup",
		"/v1/sys/remount",
		"/v1/sys/rotate",
		"/v1/sys/rotate/keyring",
		"/v1/sys/wrapping/wrap",
	}

	oidcProtectedPathRegex = regexp.MustCompile(`^identity/oidc/provider/\w(([\w-.]+)?\w)?/userinfo$`)
)

func init() {
	alwaysRedirectPaths.AddPaths([]string{
		"sys/storage/raft/snapshot",
		"sys/storage/raft/snapshot-force",
	})

	restrictedSysAPIsNonLogical.AddPaths([]string{
		"raw",
		"generate-recovery-token",
		"init",
		"seal",
		"step-down",
		"unseal",
		"health",
		"rekey-recovery-key",
		"rekey",
		"storage",
		"generate-root",
		"metrics",
		"pprof",
		"in-flight-req",
	})
}

type HandlerAnchor struct{}

func (h HandlerAnchor) Handler(props *vault.HandlerProperties) http.Handler {
	return handler(props)
}

var Handler vault.HandlerHandler = HandlerAnchor{}

type HandlerFunc func(props *vault.HandlerProperties) http.Handler

func (h HandlerFunc) Handler(props *vault.HandlerProperties) http.Handler {
	return h(props)
}

var _ vault.HandlerHandler = HandlerFunc(func(props *vault.HandlerProperties) http.Handler { return nil })

// handler returns an http.Handler for the API. This can be used on
// its own to mount the Vault API within another web server.
func handler(props *vault.HandlerProperties) http.Handler {
	core := props.Core

	// Create the muxer to handle the actual endpoints
	mux := http.NewServeMux()

	switch {
	case props.RecoveryMode:
		raw := vault.NewRawBackend(core)
		strategy := vault.GenerateRecoveryTokenStrategy(props.RecoveryToken)
		mux.Handle("/v1/sys/raw/", handleLogicalRecovery(raw, props.RecoveryToken))
		mux.Handle("/v1/sys/generate-recovery-token/attempt", handleSysGenerateRootAttempt(core, strategy))
		mux.Handle("/v1/sys/generate-recovery-token/update", handleSysGenerateRootUpdate(core, strategy))
	default:
		// Handle non-forwarded paths
		mux.Handle("/v1/sys/config/state/", handleLogicalNoForward(core))
		mux.Handle("/v1/sys/host-info", handleLogicalNoForward(core))

		mux.Handle("/v1/sys/init", handleSysInit(core))
		mux.Handle("/v1/sys/seal-status", handleSysSealStatus(core))
		mux.Handle("/v1/sys/seal", handleSysSeal(core))
		mux.Handle("/v1/sys/step-down", handleRequestForwarding(core, handleSysStepDown(core)))
		mux.Handle("/v1/sys/unseal", handleSysUnseal(core))
		mux.Handle("/v1/sys/leader", handleSysLeader(core))
		mux.Handle("/v1/sys/health", handleSysHealth(core))
		mux.Handle("/v1/sys/monitor", handleLogicalNoForward(core))

		mux.Handle("/v1/sys/generate-root/attempt", handleRequestForwarding(core,
			handleAuditNonLogical(core, handleSysGenerateRootAttempt(core, vault.GenerateStandardRootTokenStrategy))))
		mux.Handle("/v1/sys/generate-root/update", handleRequestForwarding(core,
			handleAuditNonLogical(core, handleSysGenerateRootUpdate(core, vault.GenerateStandardRootTokenStrategy))))

		// Register without unauthenticated rekey, if necessary.
		if props.ListenerConfig == nil || !props.ListenerConfig.DisableUnauthedRekeyEndpoints {
			mux.Handle("/v1/sys/rekey/init", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyInit(core, false))))
			mux.Handle("/v1/sys/rekey/update", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyUpdate(core, false))))
			mux.Handle("/v1/sys/rekey/verify", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyVerify(core, false))))
			mux.Handle("/v1/sys/rekey-recovery-key/init", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyInit(core, true))))
			mux.Handle("/v1/sys/rekey-recovery-key/update", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyUpdate(core, true))))
			mux.Handle("/v1/sys/rekey-recovery-key/verify", handleRequestForwarding(core,
				handleAuditNonLogical(core, handleSysRekeyVerify(core, true))))
		}

		mux.Handle("/v1/sys/storage/raft/bootstrap", handleSysRaftBootstrap(core))
		mux.Handle("/v1/sys/storage/raft/join", handleSysRaftJoin(core))
		mux.Handle("/v1/sys/internal/ui/feature-flags", handleSysInternalFeatureFlags(core))

		for _, path := range injectDataIntoTopRoutes {
			mux.Handle(path, handleRequestForwarding(core, handleLogicalWithInjector(core)))
		}
		mux.Handle("/v1/sys/", handleRequestForwarding(core, handleLogical(core)))
		mux.Handle("/v1/", handleRequestForwarding(core, handleLogical(core)))
		if core.UIEnabled() {
			if uiBuiltIn {
				mux.Handle("/ui/", http.StripPrefix("/ui/", gziphandler.GzipHandler(handleUIHeaders(core, handleUI(http.FileServer(&UIAssetWrapper{FileSystem: assetFS()}))))))
				mux.Handle("/robots.txt", gziphandler.GzipHandler(handleUIHeaders(core, handleUI(http.FileServer(&UIAssetWrapper{FileSystem: assetFS()})))))
			} else {
				mux.Handle("/ui/", handleUIHeaders(core, handleUIStub()))
			}
			mux.Handle("/ui", handleUIRedirect())
			mux.Handle("/", handleUIRedirect())

		}

		// Register metrics path without authentication if enabled
		if props.ListenerConfig != nil && props.ListenerConfig.Telemetry.UnauthenticatedMetricsAccess {
			mux.Handle("/v1/sys/metrics", handleMetricsUnauthenticated(core))
		} else {
			mux.Handle("/v1/sys/metrics", handleLogicalNoForward(core))
		}

		if props.ListenerConfig != nil && props.ListenerConfig.Profiling.UnauthenticatedPProfAccess {
			for _, name := range []string{"goroutine", "threadcreate", "heap", "allocs", "block", "mutex"} {
				mux.Handle("/v1/sys/pprof/"+name, pprof.Handler(name))
			}
			mux.Handle("/v1/sys/pprof/", http.HandlerFunc(pprof.Index))
			mux.Handle("/v1/sys/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
			mux.Handle("/v1/sys/pprof/profile", http.HandlerFunc(pprof.Profile))
			mux.Handle("/v1/sys/pprof/symbol", http.HandlerFunc(pprof.Symbol))
			mux.Handle("/v1/sys/pprof/trace", http.HandlerFunc(pprof.Trace))
		} else {
			mux.Handle("/v1/sys/pprof/", handleLogicalNoForward(core))
		}

		if props.ListenerConfig != nil && props.ListenerConfig.InFlightRequestLogging.UnauthenticatedInFlightAccess {
			mux.Handle("/v1/sys/in-flight-req", handleUnAuthenticatedInFlightRequest(core))
		} else {
			mux.Handle("/v1/sys/in-flight-req", handleLogicalNoForward(core))
		}
		additionalRoutes(mux, core)
	}

	// Wrap the handler in another handler to trigger all help paths.
	helpWrappedHandler := wrapHelpHandler(mux, core)
	corsWrappedHandler := wrapCORSHandler(helpWrappedHandler, core)
	quotaWrappedHandler := rateLimitQuotaWrapping(corsWrappedHandler, core)
	genericWrappedHandler := genericWrapping(core, quotaWrappedHandler, props)
	wrappedHandler := wrapMaxRequestSizeHandler(genericWrappedHandler, props)

	// Wrap the handler with PrintablePathCheckHandler to check for non-printable
	// characters in the request path.
	printablePathCheckHandler := wrappedHandler
	if !props.DisablePrintableCheck {
		printablePathCheckHandler = cleanhttp.PrintablePathCheckHandler(wrappedHandler, nil)
	}

	return printablePathCheckHandler
}

type copyResponseWriter struct {
	wrapped    http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

// newCopyResponseWriter returns an initialized newCopyResponseWriter
func newCopyResponseWriter(wrapped http.ResponseWriter) *copyResponseWriter {
	w := &copyResponseWriter{
		wrapped:    wrapped,
		body:       new(bytes.Buffer),
		statusCode: 200,
	}
	return w
}

func (w *copyResponseWriter) Header() http.Header {
	return w.wrapped.Header()
}

func (w *copyResponseWriter) Write(buf []byte) (int, error) {
	w.body.Write(buf)
	return w.wrapped.Write(buf)
}

func (w *copyResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.wrapped.WriteHeader(code)
}

func handleAuditNonLogical(core *vault.Core, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origBody := new(bytes.Buffer)
		reader := io.NopCloser(io.TeeReader(r.Body, origBody))
		r.Body = reader
		req, _, status, err := buildLogicalRequestNoAuth(w, r)
		if err != nil || status != 0 {
			respondError(w, status, err)
			return
		}
		r.Body = io.NopCloser(origBody)
		input := &logical.LogInput{
			Request: req,
		}
		ctx := namespace.RootContext(r.Context())
		err = core.AuditLogger().AuditRequest(ctx, input)
		if err != nil {
			respondError(w, status, err)
			return
		}
		cw := newCopyResponseWriter(w)
		h.ServeHTTP(cw, r)
		data := make(map[string]interface{})
		err = jsonutil.DecodeJSON(cw.body.Bytes(), &data)
		if err != nil {
			// best effort, ignore
		}
		httpResp := &logical.HTTPResponse{Data: data, Headers: cw.Header()}
		input.Response = logical.HTTPResponseToLogicalResponse(httpResp)
		err = core.AuditLogger().AuditResponse(ctx, input)
		if err != nil {
			respondError(w, status, err)
		}
	})
}

// wrapGenericHandler wraps the handler with an extra layer of handler where
// tasks that should be commonly handled for all the requests and/or responses
// are performed.
func wrapGenericHandler(core *vault.Core, h http.Handler, props *vault.HandlerProperties) http.Handler {
	var maxRequestDuration time.Duration
	if props.ListenerConfig != nil {
		maxRequestDuration = props.ListenerConfig.MaxRequestDuration
	}
	if maxRequestDuration == 0 {
		maxRequestDuration = vault.DefaultMaxRequestDuration
	}
	// Swallow this error since we don't want to pollute the logs and we also don't want to
	// return an HTTP error here. This information is best effort.
	hostname, _ := os.Hostname()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This block needs to be here so that upon sending SIGHUP, custom response
		// headers are also reloaded into the handlers.
		var customHeaders map[string][]*logical.CustomHeader
		if props.ListenerConfig != nil {
			la := props.ListenerConfig.Address
			listenerCustomHeaders := core.GetListenerCustomResponseHeaders(la)
			if listenerCustomHeaders != nil {
				customHeaders = listenerCustomHeaders.StatusCodeHeaderMap
			}
		}
		// saving start time for the in-flight requests
		inFlightReqStartTime := time.Now()

		nw := logical.NewStatusHeaderResponseWriter(w, customHeaders)

		// Set the Cache-Control header for all the responses returned
		// by Vault
		nw.Header().Set("Cache-Control", "no-store")

		// Start with the request context
		ctx := r.Context()
		var cancelFunc context.CancelFunc
		// Add our timeout, but not for the monitor or events endpoints, as they are streaming
		if strings.HasSuffix(r.URL.Path, "sys/monitor") || strings.Contains(r.URL.Path, "sys/events") {
			ctx, cancelFunc = context.WithCancel(ctx)
		} else {
			ctx, cancelFunc = context.WithTimeout(ctx, maxRequestDuration)
		}
		ctx = context.WithValue(ctx, "original_request_path", r.URL.Path)

		nsHeader := r.Header.Get(consts.NamespaceHeaderName)
		if nsHeader != "" {
			// Setting the namespace in the header to be included in the response
			nw.Header().Set(consts.NamespaceHeaderName, nsHeader)
		}

		ctx = namespace.ContextWithNamespaceHeader(ctx, nsHeader)
		r = r.WithContext(ctx)

		// Set some response headers with raft node id (if applicable) and hostname, if available
		if core.RaftNodeIDHeaderEnabled() {
			nodeID := core.GetRaftNodeID()
			if nodeID != "" {
				nw.Header().Set("X-Vault-Raft-Node-ID", nodeID)
			}
		}

		if core.HostnameHeaderEnabled() && hostname != "" {
			nw.Header().Set("X-Vault-Hostname", hostname)
		}

		isRestrictedSysAPI := nsHeader != "" && strings.HasPrefix(r.URL.Path, "/v1/sys/") &&
			restrictedSysAPIsNonLogical.HasPathSegments(r.URL.Path[len("/v1/sys/"):])

		switch {
		case isRestrictedSysAPI:
			respondError(nw, http.StatusBadRequest, fmt.Errorf("operation unavailable in namespaces"))
			cancelFunc()
			return

		case strings.HasPrefix(r.URL.Path, "/v1/"), strings.HasPrefix(r.URL.Path, "/ui"), r.URL.Path == "/robots.txt", r.URL.Path == "/":
		case strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/"):
			for _, ln := range props.AllListeners {
				if ln.Config == nil || ln.Config.TLSDisable {
					continue
				}

				if acg, ok := ln.Config.TLSCertGetter.(*listenerutil.ACMECertGetter); ok {
					if acg.HandleHTTPChallenge(w, r) {
						cancelFunc()
						return
					}
				}
			}
		default:
			respondError(nw, http.StatusNotFound, nil)
			cancelFunc()
			return
		}

		// The uuid for the request is going to be generated when a logical
		// request is generated. But, here we generate one to be able to track
		// in-flight requests, and use that to update the req data with clientID
		inFlightReqID, err := uuid.GenerateUUID()
		if err != nil {
			respondError(nw, http.StatusInternalServerError, errors.New("failed to generate an identifier for the in-flight request"))
		}
		// adding an entry to the context to enable updating in-flight
		// data with ClientID in the logical layer
		r = r.WithContext(context.WithValue(r.Context(), logical.CtxKeyInFlightRequestID{}, inFlightReqID))

		// extracting the client address to be included in the in-flight request
		var clientAddr string
		headers := r.Header[textproto.CanonicalMIMEHeaderKey("X-Forwarded-For")]
		if len(headers) == 0 {
			clientAddr = r.RemoteAddr
		} else {
			clientAddr = headers[0]
		}

		// getting the request method
		requestMethod := r.Method

		// Storing the in-flight requests. Path should include namespace as well
		core.StoreInFlightReqData(
			inFlightReqID,
			vault.InFlightReqData{
				StartTime:        inFlightReqStartTime,
				ReqPath:          r.URL.Path,
				ClientRemoteAddr: clientAddr,
				Method:           requestMethod,
			})
		defer func() {
			// Not expecting this fail, so skipping the assertion check
			core.FinalizeInFlightReqData(inFlightReqID, nw.StatusCode)
		}()

		h.ServeHTTP(nw, r)

		cancelFunc()
	})
}

func WrapForwardedForHandler(h http.Handler, l *configutil.Listener) http.Handler {
	rejectNotPresent := l.XForwardedForRejectNotPresent
	hopSkips := l.XForwardedForHopSkips
	authorizedAddrs := l.XForwardedForAuthorizedAddrs
	rejectNotAuthz := l.XForwardedForRejectNotAuthorized
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers, headersOK := r.Header[textproto.CanonicalMIMEHeaderKey("X-Forwarded-For")]
		if !headersOK || len(headers) == 0 {
			if !rejectNotPresent {
				h.ServeHTTP(w, r)
				return
			}
			respondError(w, http.StatusBadRequest, errors.New("missing x-forwarded-for header and configured to reject when not present"))
			return
		}

		host, port, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			// If not rejecting treat it like we just don't have a valid
			// header because we can't do a comparison against an address we
			// can't understand
			if !rejectNotPresent {
				h.ServeHTTP(w, r)
				return
			}
			respondError(w, http.StatusBadRequest, fmt.Errorf("error parsing client hostport: %w", err))
			return
		}

		addr, err := sockaddr.NewIPAddr(host)
		if err != nil {
			// We treat this the same as the case above
			if !rejectNotPresent {
				h.ServeHTTP(w, r)
				return
			}
			respondError(w, http.StatusBadRequest, fmt.Errorf("error parsing client address: %w", err))
			return
		}

		var found bool
		for _, authz := range authorizedAddrs {
			if authz.Contains(addr) {
				found = true
				break
			}
		}
		if !found {
			// If we didn't find it and aren't configured to reject, simply
			// don't trust it
			if !rejectNotAuthz {
				// We need to delete the X-Forwarded-For header before
				// passing it along, otherwise downstream systems will not
				// know whether or not to trust it.
				r.Header.Del(textproto.CanonicalMIMEHeaderKey("X-Forwarded-For"))

				// Now serve the request, having rejected the header.
				h.ServeHTTP(w, r)
				return
			}

			respondError(w, http.StatusBadRequest, errors.New("client address not authorized for x-forwarded-for and configured to reject connection"))
			return
		}

		// At this point we have at least one value and it's authorized

		// Split comma separated ones, which are common. This brings it in line
		// to the multiple-header case.
		var acc []string
		for _, header := range headers {
			vals := strings.Split(header, ",")
			for _, v := range vals {
				acc = append(acc, strings.TrimSpace(v))
			}
		}

		indexToUse := int64(len(acc)) - 1 - hopSkips
		if indexToUse < 0 {
			// This is likely an error in either configuration or other
			// infrastructure. We could either deny the request, or we
			// could simply not trust the value. Denying the request is
			// "safer" since if this logic is configured at all there may
			// be an assumption it can always be trusted. Given that we can
			// deny accepting the request at all if it's not from an
			// authorized address, if we're at this point the address is
			// authorized (or we've turned off explicit rejection) and we
			// should assume that what comes in should be properly
			// formatted.
			respondError(w, http.StatusBadRequest, fmt.Errorf("malformed x-forwarded-for configuration or request, hops to skip (%d) would skip before earliest chain link (chain length %d)", hopSkips, len(headers)))
			return
		}

		r.RemoteAddr = net.JoinHostPort(acc[indexToUse], port)
		h.ServeHTTP(w, r)
	})
}

// stripPrefix is a helper to strip a prefix from the path. It will
// return false from the second return value if it the prefix doesn't exist.
func stripPrefix(prefix, path string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}

	path = path[len(prefix):]
	if path == "" {
		return "", false
	}

	return path, true
}

func handleUIHeaders(core *vault.Core, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		header := w.Header()

		userHeaders, err := core.UIHeaders()
		if err != nil {
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		for k := range userHeaders {
			v := userHeaders.Get(k)
			header.Set(k, v)
		}
		h.ServeHTTP(w, req)
	})
}

func handleUI(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The fileserver handler strips trailing slashes and does a redirect.
		// We don't want the redirect to happen so we preemptively trim the slash
		// here.
		req.URL.Path = strings.TrimSuffix(req.URL.Path, "/")
		h.ServeHTTP(w, req)
	})
}

func handleUIStub() http.Handler {
	stubHTML := `
	<!DOCTYPE html>
	<html>
	<style>
	body {
	color: #1F2124;
	font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", "Roboto", "Oxygen", "Ubuntu", "Cantarell", "Fira Sans", "Droid Sans", "Helvetica Neue", sans-serif;
	}

	.wrapper {
	display: flex;
	justify-content: center;
	align-items: center;
	height: 500px;
	}

	.content ul {
	line-height: 1.5;
	}

	a {
	color: #1563ff;
	text-decoration: none;
	}

	.header {
	display: flex;
	color: #6a7786;
	align-items: center;
	}

	.header svg {
	padding-right: 12px;
	}

	.alert {
	transform: scale(0.07);
	fill: #6a7786;
	}

	h1 {
	font-weight: 500;
	}

	p {
	margin-top: 0px;
	}
	</style>
	<div class="wrapper">
	<div class="content">
	<div class="header">
	<svg width="36px" height="36px" viewBox="0 0 36 36" xmlns="http://www.w3.org/2000/svg">
	<path class="alert" d="M476.7 422.2L270.1 72.7c-2.9-5-8.3-8.7-14.1-8.7-5.9 0-11.3 3.7-14.1 8.7L35.3 422.2c-2.8 5-4.8 13-1.9 17.9 2.9 4.9 8.2 7.9 14 7.9h417.1c5.8 0 11.1-3 14-7.9 3-4.9 1-13-1.8-17.9zM288 400h-64v-48h64v48zm0-80h-64V176h64v144z"/>
	</svg>
	<h1>OpenBao UI is not available in this binary.</h1>
	</div>
	<p>To get the beta OpenBao UI do the following:</p>
	<ul>
	<li>Run <code>make dev-ui</code> to create a development binary with the UI.
	</ul>
	<p>Contributions to help improve the <a href="https://openbao.org/">OpenBao</a> UI are welcome!</p>
	</div>
	</div>
	</html>
	`
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(stubHTML))
	})
}

func handleUIRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/ui/", 307)
	})
}

type UIAssetWrapper struct {
	FileSystem http.FileSystem
}

func (fsw *UIAssetWrapper) Open(name string) (http.File, error) {
	file, err := fsw.FileSystem.Open(name)
	if err == nil {
		return file, nil
	}
	// serve index.html instead of 404ing
	if errors.Is(err, fs.ErrNotExist) {
		file, err := fsw.FileSystem.Open("index.html")
		return file, err
	}
	return nil, err
}

func parseQuery(values url.Values) map[string]interface{} {
	data := map[string]interface{}{}
	for k, v := range values {
		// Skip the help key as this is a reserved parameter
		if k == "help" {
			continue
		}

		switch {
		case len(v) == 0:
		case len(v) == 1:
			data[k] = v[0]
		default:
			data[k] = v
		}
	}

	if len(data) > 0 {
		return data
	}
	return nil
}

func parseJSONRequest(r *http.Request, w http.ResponseWriter, out interface{}) (io.ReadCloser, error) {
	// Limit the maximum number of bytes to MaxRequestSize to protect
	// against an indefinite amount of data being read.
	reader := r.Body
	err := jsonutil.DecodeJSONFromReader(reader, out)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to parse JSON input: %w", err)
	}
	return nil, err
}

// parseFormRequest parses values from a form POST.
//
// A nil map will be returned if the format is empty or invalid.
func parseFormRequest(r *http.Request) (map[string]interface{}, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}

	var data map[string]interface{}

	if len(r.PostForm) != 0 {
		data = make(map[string]interface{}, len(r.PostForm))
		for k, v := range r.PostForm {
			switch len(v) {
			case 0:
			case 1:
				data[k] = v[0]
			default:
				// Almost anywhere taking in a string list can take in comma
				// separated values, and really this is super niche anyways
				data[k] = strings.Join(v, ",")
			}
		}
	}

	return data, nil
}

// handleRequestForwarding determines whether to forward a request or not,
// falling back on the older behavior of redirecting the client
func handleRequestForwarding(core *vault.Core, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Note: in an HA setup, this call will also ensure that connections to
		// the leader are set up, as that happens once the advertised cluster
		// values are read during this function
		isLeader, leaderAddr, _, err := core.Leader()
		if err != nil {
			if err == vault.ErrHANotEnabled {
				// Standalone node, serve request normally
				handler.ServeHTTP(w, r)
				return
			}
			// Some internal error occurred
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		if isLeader {
			// No forwarding needed, we're leader
			handler.ServeHTTP(w, r)
			return
		}
		if leaderAddr == "" {
			respondError(w, http.StatusInternalServerError, errors.New("local node not active but active cluster node not found"))
			return
		}

		forwardRequest(core, w, r)
	})
}

func forwardRequest(core *vault.Core, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(vault.IntNoForwardingHeaderName) != "" {
		respondStandby(core, w, r.URL)
		return
	}

	if r.Header.Get(NoRequestForwardingHeaderName) != "" {
		// Forwarding explicitly disabled, fall back to previous behavior
		core.Logger().Debug("handleRequestForwarding: forwarding disabled by client request")
		respondStandby(core, w, r.URL)
		return
	}

	if alwaysRedirectPaths.HasPath(r.URL.Path[len("/v1/"):]) {
		respondStandby(core, w, r.URL)
		return
	}

	// Attempt forwarding the request. If we cannot forward -- perhaps it's
	// been disabled on the active node -- this will return with an
	// ErrCannotForward and we simply fall back
	statusCode, header, retBytes, err := core.ForwardRequest(r)
	if err != nil {
		if err == vault.ErrCannotForward {
			core.Logger().Debug("cannot forward request (possibly disabled on active node), falling back")
		} else {
			core.Logger().Error("forward request error", "error", err)
		}

		// Fall back to redirection
		respondStandby(core, w, r.URL)
		return
	}

	for k, v := range header {
		w.Header()[k] = v
	}

	w.WriteHeader(statusCode)
	w.Write(retBytes)
}

// request is a helper to perform a request and properly exit in the
// case of an error.
func request(core *vault.Core, w http.ResponseWriter, rawReq *http.Request, r *logical.Request) (*logical.Response, bool, bool) {
	resp, err := core.HandleRequest(rawReq.Context(), r)
	if errwrap.Contains(err, consts.ErrStandby.Error()) {
		respondStandby(core, w, rawReq.URL)
		return resp, false, false
	}
	if err != nil && errwrap.Contains(err, logical.ErrPerfStandbyPleaseForward.Error()) {
		return nil, false, true
	}

	if resp != nil && len(resp.Headers) > 0 {
		// Set this here so it will take effect regardless of any other type of
		// response processing
		header := w.Header()
		for k, v := range resp.Headers {
			for _, h := range v {
				header.Add(k, h)
			}
		}

		switch {
		case resp.Secret != nil,
			resp.Auth != nil,
			len(resp.Data) > 0,
			resp.Redirect != "",
			len(resp.Warnings) > 0,
			resp.WrapInfo != nil:
			// Nothing, resp has data

		default:
			// We have an otherwise totally empty response except for headers,
			// so nil out the response now that the headers are written out
			resp = nil
		}
	}

	// If vault's core has already written to the response writer do not add any
	// additional output. Headers have already been sent. If the response writer
	// is set but has not been written to it likely means there was some kind of
	// error
	if r.ResponseWriter != nil && r.ResponseWriter.Written() {
		return nil, true, false
	}

	if respondErrorCommon(w, r, resp, err) {
		return resp, false, false
	}

	return resp, true, false
}

// respondStandby is used to trigger a redirect in the case that this Vault is currently a hot standby
func respondStandby(core *vault.Core, w http.ResponseWriter, reqURL *url.URL) {
	// Request the leader address
	_, redirectAddr, _, err := core.Leader()
	if err != nil {
		if err == vault.ErrHANotEnabled {
			// Standalone node, serve 503
			err = errors.New("node is not active")
			respondError(w, http.StatusServiceUnavailable, err)
			return
		}

		respondError(w, http.StatusInternalServerError, err)
		return
	}

	// If there is no leader, generate a 503 error
	if redirectAddr == "" {
		err = errors.New("no active Vault instance found")
		respondError(w, http.StatusServiceUnavailable, err)
		return
	}

	// Parse the redirect location
	redirectURL, err := url.Parse(redirectAddr)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	// Generate a redirect URL
	finalURL := url.URL{
		Scheme:   redirectURL.Scheme,
		Host:     redirectURL.Host,
		Path:     reqURL.Path,
		RawQuery: reqURL.RawQuery,
	}

	// Ensure there is a scheme, default to https
	if finalURL.Scheme == "" {
		finalURL.Scheme = "https"
	}

	// If we have an address, redirect! We use a 307 code
	// because we don't actually know if its permanent and
	// the request method should be preserved.
	w.Header().Set("Location", finalURL.String())
	w.WriteHeader(307)
}

// getTokenFromReq parse headers of the incoming request to extract token if
// present it accepts Authorization Bearer (RFC6750) and X-Vault-Token header.
// Returns true if the token was sourced from a Bearer header.
func getTokenFromReq(r *http.Request) (string, bool) {
	if token := r.Header.Get(consts.AuthHeaderName); token != "" {
		return token, false
	}
	if headers, ok := r.Header["Authorization"]; ok {
		// Reference for Authorization header format: https://tools.ietf.org/html/rfc7236#section-3

		// If string does not start by 'Bearer ', it is not one we would use,
		// but might be used by plugins
		for _, v := range headers {
			if !strings.HasPrefix(v, "Bearer ") {
				continue
			}
			return strings.TrimSpace(v[7:]), true
		}
	}
	return "", false
}

// requestAuth adds the token to the logical.Request if it exists.
func requestAuth(r *http.Request, req *logical.Request) {
	// Attach the header value if we have it
	token, fromAuthzHeader := getTokenFromReq(r)
	if token != "" {
		req.ClientToken = token
		req.ClientTokenSource = logical.ClientTokenFromVaultHeader
		if fromAuthzHeader {
			req.ClientTokenSource = logical.ClientTokenFromAuthzHeader
		}

	}
}

func requestPolicyOverride(r *http.Request, req *logical.Request) error {
	raw := r.Header.Get(PolicyOverrideHeaderName)
	if raw == "" {
		return nil
	}

	override, err := parseutil.ParseBool(raw)
	if err != nil {
		return err
	}

	req.PolicyOverride = override
	return nil
}

// requestWrapInfo adds the WrapInfo value to the logical.Request if wrap info exists
func requestWrapInfo(r *http.Request, req *logical.Request) (*logical.Request, error) {
	// First try for the header value
	wrapTTL := r.Header.Get(WrapTTLHeaderName)
	if wrapTTL == "" {
		return req, nil
	}

	// If it has an allowed suffix parse as a duration string
	dur, err := parseutil.ParseDurationSecond(wrapTTL)
	if err != nil {
		return req, err
	}
	if int64(dur) < 0 {
		return req, errors.New("requested wrap ttl cannot be negative")
	}

	req.WrapInfo = &logical.RequestWrapInfo{
		TTL: dur,
	}

	wrapFormat := r.Header.Get(WrapFormatHeaderName)
	switch wrapFormat {
	case "jwt":
		req.WrapInfo.Format = "jwt"
	}

	return req, nil
}

// parseMFAHeader parses the MFAHeaderName in the request headers and organizes
// them with MFA method name as the index.
func parseMFAHeader(req *logical.Request) error {
	if req == nil {
		return errors.New("request is nil")
	}

	if req.Headers == nil {
		return nil
	}

	// Reset and initialize the credentials in the request
	req.MFACreds = make(map[string][]string)

	for _, mfaHeaderValue := range req.Headers[canonicalMFAHeaderName] {
		// Skip the header with no value in it
		if mfaHeaderValue == "" {
			continue
		}

		// Handle the case where only method name is mentioned and no value
		// is supplied
		if !strings.Contains(mfaHeaderValue, ":") {
			// Mark the presence of method name, but set an empty set to it
			// indicating that there were no values supplied for the method
			if req.MFACreds[mfaHeaderValue] == nil {
				req.MFACreds[mfaHeaderValue] = []string{}
			}
			continue
		}

		shardSplits := strings.SplitN(mfaHeaderValue, ":", 2)
		if shardSplits[0] == "" {
			return fmt.Errorf("invalid data in header %q; missing method name or ID", MFAHeaderName)
		}

		if shardSplits[1] == "" {
			return fmt.Errorf("invalid data in header %q; missing method value", MFAHeaderName)
		}

		req.MFACreds[shardSplits[0]] = append(req.MFACreds[shardSplits[0]], shardSplits[1])
	}

	return nil
}

// isForm tries to determine whether the request should be
// processed as a form or as JSON.
//
// Virtually all existing use cases have assumed processing as JSON,
// and there has not been a Content-Type requirement in the API. In order to
// maintain backwards compatibility, this will err on the side of JSON.
// The request will be considered a form only if:
//
//  1. The content type is "application/x-www-form-urlencoded"
//  2. The start of the request doesn't look like JSON. For this test we
//     we expect the body to begin with { or [, ignoring leading whitespace.
func isForm(head []byte, contentType string) bool {
	contentType, _, err := mime.ParseMediaType(contentType)

	if err != nil || contentType != "application/x-www-form-urlencoded" {
		return false
	}

	// Look for the start of JSON or not-JSON, skipping any insignificant
	// whitespace (per https://tools.ietf.org/html/rfc7159#section-2).
	for _, c := range head {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '[', '{': // JSON
			return false
		default: // not JSON
			return true
		}
	}

	return true
}

func respondError(w http.ResponseWriter, status int, err error) {
	logical.RespondError(w, status, err)
}

func respondErrorAndData(w http.ResponseWriter, status int, data interface{}, err error) {
	logical.RespondErrorAndData(w, status, data, err)
}

func respondErrorCommon(w http.ResponseWriter, req *logical.Request, resp *logical.Response, err error) bool {
	statusCode, newErr := logical.RespondErrorCommon(req, resp, err)
	if newErr == nil && statusCode == 0 {
		return false
	}

	// If ErrPermissionDenied occurs for OIDC protected resources (e.g., userinfo),
	// then respond with a JSON error format that complies with the specification.
	// This prevents the JSON error format from changing to a Vault-y format (i.e.,
	// the format that results from respondError) after an OIDC access token expires.
	if oidcPermissionDenied(req.Path, err) {
		respondOIDCPermissionDenied(w)
		return true
	}

	if resp != nil {
		if data := resp.Data["data"]; data != nil {
			respondErrorAndData(w, statusCode, data, newErr)
			return true
		}
	}
	respondError(w, statusCode, newErr)
	return true
}

func respondOk(w http.ResponseWriter, body interface{}) {
	w.Header().Set("Content-Type", "application/json")

	if body == nil {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.Encode(body)
	}
}

// oidcPermissionDenied returns true if the given path matches the
// UserInfo Endpoint published by Vault OIDC providers and the given
// error is a logical.ErrPermissionDenied.
func oidcPermissionDenied(path string, err error) bool {
	return errwrap.Contains(err, logical.ErrPermissionDenied.Error()) &&
		oidcProtectedPathRegex.MatchString(path)
}

// respondOIDCPermissionDenied writes a response to the given w for
// permission denied errors (expired token) on resources protected
// by OIDC access tokens. Currently, the UserInfo Endpoint is the only
// protected resource. See the following specifications for details:
//   - https://openid.net/specs/openid-connect-core-1_0.html#UserInfoError
//   - https://datatracker.ietf.org/doc/html/rfc6750#section-3.1
func respondOIDCPermissionDenied(w http.ResponseWriter) {
	errorCode := "invalid_token"
	errorDescription := logical.ErrPermissionDenied.Error()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer error=%q,error_description=%q",
		errorCode, errorDescription))
	w.WriteHeader(http.StatusUnauthorized)

	var oidcResponse struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	oidcResponse.Error = errorCode
	oidcResponse.ErrorDescription = errorDescription

	enc := json.NewEncoder(w)
	enc.Encode(oidcResponse)
}
