package tool

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// credentialRedirect is the redirect policy of a client that carries an API
// key. Go's default policy drops Authorization and Cookie on a redirect to
// another host, but keeps every other header (X-Subscription-Token,
// x-api-key, ...) and, on 307 and 308, resends the body (where some search
// APIs put the key). On a redirect to another host it therefore:
//
//   - drops every header but a few that never carry a credential;
//   - refuses a redirect that resends the request body;
//
// and it never follows https to plain http, on any host. A redirect on the
// same host and port is followed as before, up to ten hops.
func credentialRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	from := via[0].URL
	if from.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect from https to %s (%s): the request carries an API key", req.URL.Scheme, req.URL.Host)
	}
	if strings.EqualFold(req.URL.Hostname(), from.Hostname()) && req.URL.Port() == from.Port() {
		return nil
	}
	if req.GetBody != nil && req.Method != http.MethodGet && req.Method != http.MethodHead {
		return fmt.Errorf("refusing a redirect from %s to %s that resends the request body (it may carry an API key)", from.Host, req.URL.Host)
	}
	for k := range req.Header {
		if !redirectSafeHeaders[http.CanonicalHeaderKey(k)] {
			req.Header.Del(k)
		}
	}
	return nil
}

// redirectSafeHeaders are kept on a redirect to another host.
var redirectSafeHeaders = map[string]bool{
	"Accept": true, "Accept-Encoding": true, "Accept-Language": true, "User-Agent": true,
	"Content-Type": true, "Cache-Control": true, "Pragma": true,
}
