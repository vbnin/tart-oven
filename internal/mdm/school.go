package mdm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	schoolEnrollPath          = "/enroll/"
	schoolStartEnrollmentPath = "/enroll/startEnrollment.html"
	schoolCSRFMetaName        = "csrf-token"

	// schoolFetchTimeout bounds a whole three-step handshake. It is applied as a
	// context deadline inside FetchProfile rather than as the client's Timeout,
	// because http.Client.Timeout is per-request: three requests under a 30s client
	// can still take 90s, and a caller that injects its own client (the Manager
	// seams in internal/server do exactly that) may supply one with no timeout at
	// all. The daemon reaches a tenant's server over the internet from a task
	// goroutine, so a server that accepts the connection and never writes must not
	// be able to leave a task banner up indefinitely.
	schoolFetchTimeout = 30 * time.Second

	// schoolMaxRedirects matches net/http's own default redirect limit.
	schoolMaxRedirects = 10
)

type SchoolInput struct {
	BaseURL  string
	Network  string
	Location string
}

// SchoolFetcher downloads an enrollment profile from a Jamf School server.
//
// Unlike the Jamf Pro path, which generates a profile locally from a template,
// there is no way to produce a School profile offline: the server mints one, and
// each call mints a FRESH PKCS#12 client identity. So a result must never be
// cached or shared between VMs — two VMs holding one profile means two devices
// presenting one MDM identity, which collides their records in Jamf School. Fetch
// once per VM, per request, and never persist the bytes.
type SchoolFetcher interface {
	FetchProfile(ctx context.Context, in SchoolInput) ([]byte, error)
}

type schoolFetcher struct {
	client  *http.Client
	timeout time.Duration
}

// NewSchoolFetcher returns a fetcher using client, or a default client when
// client is nil. The handshake timeout is applied either way; see
// schoolFetchTimeout.
func NewSchoolFetcher(client *http.Client) SchoolFetcher {
	if client == nil {
		client = &http.Client{}
	}
	return &schoolFetcher{client: client, timeout: schoolFetchTimeout}
}

// FetchProfile replays the three requests a browser makes to enroll a device:
// the enrollment landing page (for a session cookie and a CSRF token), the
// startEnrollment.html call that mints the profile, and the download itself.
//
// This is a scrape, not a documented API, so it breaks if Jamf changes the
// enrollment page markup or the startEnrollment response shape. Both failures
// surface as a StageError naming the step that broke rather than as a generic
// error, because "which of the three steps failed" is the only diagnostic a user
// can act on.
//
// No response body ever reaches a returned error. The profile embeds a PKCS#12
// identity and that identity's cleartext password; the landing page carries a
// session cookie and CSRF token; and the network and location IDs, which travel
// in the landing URL's query string, are stored as write-only secrets. Errors
// from here reach task output and HTTP responses, so they describe the shape of
// a failure only.
func (f *schoolFetcher) FetchProfile(ctx context.Context, in SchoolInput) ([]byte, error) {
	base, err := schoolBaseURL(in)
	if err != nil {
		return nil, err
	}

	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, newStageError(StageLanding, "create enrollment cookie jar", err)
	}
	// A per-fetch jar and redirect policy, on a shallow copy of the caller's
	// client: the session established below must not outlive this call or be
	// shared with another fetch, and the caller's client is not ours to mutate.
	// http.Client carries no locks, so copying it is safe, and sharing its
	// Transport is what keeps connection pooling.
	client := *f.client
	client.Jar = jar
	client.CheckRedirect = refuseCrossHostRedirect

	csrfToken, err := fetchSchoolCSRFToken(ctx, &client, base, in)
	if err != nil {
		return nil, err
	}
	profileURL, err := startSchoolEnrollment(ctx, &client, base, in, csrfToken)
	if err != nil {
		return nil, err
	}
	return downloadSchoolProfile(ctx, &client, profileURL)
}

// refuseCrossHostRedirect follows redirects within the configured host but
// refuses any that leaves it.
//
// net/http strips Cookie and Authorization when a redirect crosses origins, but
// it keeps custom headers and the Referer, and on a 307 or 308 it re-sends the
// request body. Following a cross-host redirect would therefore hand a third
// party the X-CSRF-TOKEN header, the landing URL (whose query string carries the
// network and location IDs, stored as write-only secrets) by way of Referer, and
// the whole multipart form. That is not hypothetical: a tenant fronting Jamf
// School with SSO can answer an unauthenticated /enroll/ with a redirect to an
// identity provider on another host.
func refuseCrossHostRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= schoolMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", schoolMaxRedirects)
	}
	if len(via) > 0 && !strings.EqualFold(request.URL.Host, via[0].URL.Host) {
		return errors.New("refusing to follow a redirect away from the configured Jamf School host")
	}
	// The download carries a private key and its password, so a redirect must
	// not drop from https to plain http, even on the same host.
	if len(via) > 0 && via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
		return errors.New("refusing to follow a redirect from https to http")
	}
	return nil
}

// sameSchoolHost reports whether two URLs name the same host and port. A missing
// port means the scheme's default (443 for https).
func sameSchoolHost(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func schoolBaseURL(in SchoolInput) (*url.URL, error) {
	normalized, err := NormalizeJamfBaseURL(in.BaseURL)
	if err != nil {
		return nil, &StageError{Stage: StageConfiguration, Err: err}
	}
	if normalized == "" {
		return nil, &StageError{Stage: StageConfiguration, Err: errors.New("Jamf School server base URL is required")}
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, &StageError{Stage: StageConfiguration, Err: errors.New("Jamf School server base URL could not be parsed")}
	}
	// The profile this server hands out holds a private key and its cleartext
	// password, and the enrollment calls carry a session cookie and CSRF token,
	// so none of it may cross the network unencrypted. (Jamf Pro never had this
	// exposure: its profile is generated locally.)
	if base.Scheme != "https" {
		return nil, &StageError{Stage: StageConfiguration, Err: errors.New("Jamf School server base URL must use https")}
	}
	if strings.TrimSpace(in.Network) == "" {
		return nil, &StageError{Stage: StageConfiguration, Err: errors.New("Jamf School network is required")}
	}
	if strings.TrimSpace(in.Location) == "" {
		return nil, &StageError{Stage: StageConfiguration, Err: errors.New("Jamf School location is required")}
	}
	return base, nil
}

// Step 1: the enrollment landing page. The response sets the session cookie that
// the next two requests depend on, and carries the CSRF token in a meta tag.
func fetchSchoolCSRFToken(ctx context.Context, client *http.Client, base *url.URL, in SchoolInput) (string, error) {
	landing := *base
	landing.Path = schoolPath(base, schoolEnrollPath)
	landing.RawQuery = url.Values{
		"network":  {in.Network},
		"location": {in.Location},
	}.Encode()

	const operation = "request Jamf School enrollment page"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, landing.String(), nil)
	if err != nil {
		return "", &StageError{Stage: StageLanding, Err: fmt.Errorf("%s: build request", operation)}
	}
	page, err := doSchoolRequest(client, request, StageLanding, operation)
	if err != nil {
		return "", err
	}
	token, err := extractSchoolCSRFToken(page)
	if err != nil {
		return "", &StageError{Stage: StageLanding, Err: err}
	}
	return token, nil
}

// extractSchoolCSRFToken reads <meta name="csrf-token" content="..."> from the
// enrollment page. Parsed with golang.org/x/net/html rather than matched with a
// regexp: attribute order, quoting and whitespace in that tag are not ours to
// predict, and a regexp that silently stops matching would look like an
// authentication failure three steps later.
func extractSchoolCSRFToken(page []byte) (string, error) {
	document, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		// html.Parse is forgiving enough that this is close to unreachable; return a
		// shape-only message, because the parser's own message can quote the markup.
		return "", errors.New("Jamf School enrollment page could not be parsed as HTML")
	}
	var token string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if token != "" {
			return
		}
		if node.Type == html.ElementNode && node.Data == "meta" {
			var name, content string
			for _, attribute := range node.Attr {
				switch attribute.Key {
				case "name":
					name = attribute.Val
				case "content":
					content = attribute.Val
				}
			}
			if strings.EqualFold(name, schoolCSRFMetaName) && content != "" {
				token = content
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if token == "" {
		return "", errors.New(`Jamf School enrollment page has no <meta name="csrf-token"> value; the enrollment page markup may have changed`)
	}
	return token, nil
}

// Step 2: startEnrollment.html mints the profile and answers with its URL.
func startSchoolEnrollment(ctx context.Context, client *http.Client, base *url.URL, in SchoolInput, csrfToken string) (*url.URL, error) {
	const operation = "start Jamf School enrollment"

	// multipart/form-data, not urlencoded: the enrollment endpoint rejects a
	// urlencoded body. The empty fields are sent because the browser sends them.
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for _, field := range []struct{ name, value string }{
		{"userId", ""},
		{"hash", ""},
		{"network", in.Network},
		{"appleid", ""},
		{"location", in.Location},
	} {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: build enrollment form", operation)}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: build enrollment form", operation)}
	}

	start := *base
	start.Path = schoolPath(base, schoolStartEnrollmentPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, start.String(), &form)
	if err != nil {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: build request", operation)}
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-CSRF-TOKEN", csrfToken)
	request.Header.Set("X-Requested-With", "XMLHttpRequest")

	body, err := doSchoolRequest(client, request, StageSession, operation)
	if err != nil {
		return nil, err
	}

	// Decoding with encoding/json also undoes the backslash-escaped slashes in the
	// URL, which the bash prototype stripped with `sed 's/\\\//\//g'`.
	var payload struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// json's own error quotes the offending input, so it is not wrapped.
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: server response was not the expected JSON", operation)}
	}
	if strings.TrimSpace(payload.Profile) == "" {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: server response JSON has no %q URL; the enrollment API may have changed", operation, "profile")}
	}
	reference, err := url.Parse(strings.TrimSpace(payload.Profile))
	if err != nil {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: server response JSON %q value is not a valid URL", operation, "profile")}
	}
	// Resolving against the base accepts both the absolute URL the server sends
	// today and a bare path, without assuming either.
	profileURL := base.ResolveReference(reference)
	if profileURL.Scheme != "https" {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: server response JSON %q value is not an https URL", operation, "profile")}
	}
	// The download is a private key. A response that points it at another host
	// (a compromised or misconfigured server, or an injected reply) must not
	// make us send it there. Unconfirmed against a real tenant: if Jamf School
	// serves profiles from a different host, this is the line to widen.
	if !sameSchoolHost(profileURL, base) {
		return nil, &StageError{Stage: StageSession, Err: fmt.Errorf("%s: server response JSON %q value is on a different host than the configured Jamf School server", operation, "profile")}
	}
	return profileURL, nil
}

// Step 3: download the profile and sanity-check its shape. The bytes are returned
// unchanged — macOS installs a signed .mobileconfig natively and the install
// dialog then reads "Verified", so unwrapping or re-signing would be a downgrade.
func downloadSchoolProfile(ctx context.Context, client *http.Client, profileURL *url.URL) ([]byte, error) {
	const operation = "download Jamf School enrollment profile"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL.String(), nil)
	if err != nil {
		return nil, &StageError{Stage: StageDownload, Err: fmt.Errorf("%s: build request", operation)}
	}
	profile, err := doSchoolRequest(client, request, StageDownload, operation)
	if err != nil {
		return nil, err
	}
	if err := ValidateSchoolProfile(profile); err != nil {
		return nil, &StageError{Stage: StageDownload, Err: err}
	}
	return profile, nil
}

func schoolPath(base *url.URL, suffix string) string {
	return strings.TrimSuffix(base.Path, "/") + suffix
}

func doSchoolRequest(client *http.Client, request *http.Request, stage Stage, operation string) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, newStageError(stage, operation, stripSchoolRequestURL(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// StatusText, not the server's own reason phrase, which is server-controlled
		// text we would be echoing.
		return nil, &StageError{Stage: stage, Err: fmt.Errorf("%s: server returned HTTP %d %s",
			operation, response.StatusCode, http.StatusText(response.StatusCode))}
	}
	body, err := readCappedSchoolBody(response.Body)
	if err != nil {
		return nil, newStageError(stage, operation, err)
	}
	return body, nil
}

// readCappedSchoolBody reads at most maxSchoolProfileBytes, so a server that
// streams an unbounded body cannot exhaust memory. A real profile is around
// 9.7 KB and the other two responses are far smaller.
func readCappedSchoolBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxSchoolProfileBytes+1))
	if err != nil {
		return nil, errors.New("read response body")
	}
	if len(data) > maxSchoolProfileBytes {
		return nil, fmt.Errorf("response body is over the %d byte limit", maxSchoolProfileBytes)
	}
	return data, nil
}

// stripSchoolRequestURL unwraps the *url.Error that http.Client.Do returns. Its
// message embeds the request URL, and the enrollment landing URL carries the
// network and location IDs, which are stored as write-only secrets. Unwrapping
// drops the URL while keeping the cause — so errors.Is(err, context.Canceled)
// still works for a caller that wants to distinguish a cancellation.
func stripSchoolRequestURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
