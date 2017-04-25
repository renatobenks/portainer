package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/portainer/portainer"
	"github.com/portainer/portainer/crypto"
	httperror "github.com/portainer/portainer/http/error"
)

// proxyFactory is a factory to create reverse proxies to Docker endpoints
type proxyFactory struct {
	ResourceControlService portainer.ResourceControlService
	TeamService            portainer.TeamService
}

func (factory *proxyFactory) newHTTPProxy(u *url.URL) http.Handler {
	u.Scheme = "http"
	return factory.newSingleHostReverseProxyWithHostHeader(u)
}

func (factory *proxyFactory) newHTTPSProxy(u *url.URL, endpoint *portainer.Endpoint) (http.Handler, error) {
	u.Scheme = "https"
	proxy := factory.newSingleHostReverseProxyWithHostHeader(u)
	config, err := crypto.CreateTLSConfiguration(endpoint.TLSCACertPath, endpoint.TLSCertPath, endpoint.TLSKeyPath)
	if err != nil {
		return nil, err
	}

	proxy.Transport.(*proxyTransport).transport.TLSClientConfig = config
	return proxy, nil
}

func (factory *proxyFactory) newSocketProxy(path string) http.Handler {
	return &unixSocketHandler{path, &proxyTransport{
		ResourceControlService: factory.ResourceControlService,
		TeamService:            factory.TeamService,
	}}
}

// unixSocketHandler represents a handler to proxy HTTP requests via a unix:// socket
type unixSocketHandler struct {
	path      string
	transport *proxyTransport
}

func (h *unixSocketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := net.Dial("unix", h.path)
	if err != nil {
		httperror.WriteErrorResponse(w, err, http.StatusInternalServerError, nil)
		return
	}
	c := httputil.NewClientConn(conn, nil)
	defer c.Close()

	res, err := c.Do(r)
	if err != nil {
		httperror.WriteErrorResponse(w, err, http.StatusInternalServerError, nil)
		return
	}
	defer res.Body.Close()

	err = h.transport.proxyDockerRequests(r, res)
	if err != nil {
		httperror.WriteErrorResponse(w, err, http.StatusInternalServerError, nil)
		return
	}

	for k, vv := range res.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if _, err := io.Copy(w, res.Body); err != nil {
		httperror.WriteErrorResponse(w, err, http.StatusInternalServerError, nil)
	}
}

// singleJoiningSlash from golang.org/src/net/http/httputil/reverseproxy.go
// included here for use in NewSingleHostReverseProxyWithHostHeader
// because its used in NewSingleHostReverseProxy from golang.org/src/net/http/httputil/reverseproxy.go
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// NewSingleHostReverseProxyWithHostHeader is based on NewSingleHostReverseProxy
// from golang.org/src/net/http/httputil/reverseproxy.go and merely sets the Host
// HTTP header, which NewSingleHostReverseProxy deliberately preserves.
// It also adds an extra Transport to the proxy to allow Portainer to rewrite the responses.
func (factory *proxyFactory) newSingleHostReverseProxyWithHostHeader(target *url.URL) *httputil.ReverseProxy {
	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		req.Host = req.URL.Host
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
	}
	transport := &proxyTransport{
		ResourceControlService: factory.ResourceControlService,
		TeamService:            factory.TeamService,
		transport:              &http.Transport{},
	}
	return &httputil.ReverseProxy{Director: director, Transport: transport}
}