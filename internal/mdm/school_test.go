package mdm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Obviously-fake identifiers. A real tenant hostname, network or location ID must
// never land in this repository, not even as a test fixture.
const (
	fakeCSRFToken             = "fake-csrf-token-0000"
	fakeNetworkID             = "fake-network-0001"
	fakeLocationID            = "fake-location-0002"
	fakeSessionCookie         = "fake-enroll-session"
	fakeSessionValue          = "fake-session-value-0003"
	fakeProfilePath           = "/enroll/profile/fake-profile-path"
	fakeRedirectedProfilePath = "/enroll/profile/fake-redirected-path"
	bodySecretSentinel        = "RESPONSE-BODY-MUST-NOT-LEAK"
)

const schoolLandingHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width">
<meta name="csrf-token" content="%s">
<title>Device Enrollment</title>
</head><body><form id="enrollForm"></form>` + bodySecretSentinel + `</body></html>`

type schoolRecordedRequest struct {
	method      string
	path        string
	query       url.Values
	header      http.Header
	cookies     []*http.Cookie
	form        map[string]string
	contentType string
}

func (r schoolRecordedRequest) cookie(name string) (string, bool) {
	for _, cookie := range r.cookies {
		if cookie.Name == name {
			return cookie.Value, true
		}
	}
	return "", false
}

type schoolTestServer struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []schoolRecordedRequest

	// Knobs; defaults describe the happy path.
	pathPrefix        string
	landingStatus     int
	landingBody       []byte
	landingRedirectTo string
	blockLanding      chan struct{}
	csrfToken         string
	omitCSRF          bool
	sessionStatus     int
	sessionBody       []byte
	sessionRedirectTo string
	profileRedirectTo string
	profileURL        string
	omitProfileInJSON bool
	profileStatus     int
	profile           []byte
	beforeLanding     func()
	beforeSession     func()
	beforeProfile     func()
}

func newSchoolTestServer(t *testing.T) *schoolTestServer {
	t.Helper()
	return newSchoolTestServerAt(t, "")
}

// newSchoolTestServerAt mounts the enrollment endpoints under pathPrefix, so a
// Jamf School reached at https://host/jss can be exercised as well as one at the
// root.
func newSchoolTestServerAt(t *testing.T, pathPrefix string) *schoolTestServer {
	t.Helper()
	testServer := &schoolTestServer{
		t:             t,
		pathPrefix:    pathPrefix,
		landingStatus: http.StatusOK,
		csrfToken:     fakeCSRFToken,
		sessionStatus: http.StatusOK,
		profileStatus: http.StatusOK,
		profile:       syntheticSignedDataCMS(t),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pathPrefix+"/enroll/startEnrollment.html", testServer.handleSession)
	mux.HandleFunc(pathPrefix+fakeProfilePath, testServer.handleProfile)
	mux.HandleFunc(pathPrefix+fakeRedirectedProfilePath, testServer.handleProfileRedirect)
	mux.HandleFunc(pathPrefix+"/enroll/", testServer.handleLanding)
	testServer.server = httptest.NewServer(mux)
	t.Cleanup(testServer.server.Close)
	return testServer
}

func (s *schoolTestServer) baseURL() string { return s.server.URL + s.pathPrefix }

func (s *schoolTestServer) record(r *http.Request) {
	recorded := schoolRecordedRequest{
		method:      r.Method,
		path:        r.URL.Path,
		query:       r.URL.Query(),
		header:      r.Header.Clone(),
		cookies:     r.Cookies(),
		contentType: r.Header.Get("Content-Type"),
		form:        map[string]string{},
	}
	if strings.HasPrefix(recorded.contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			s.t.Errorf("parse multipart form: %v", err)
		} else {
			for name, values := range r.MultipartForm.Value {
				recorded.form[name] = strings.Join(values, ",")
			}
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, recorded)
	s.mu.Unlock()
}

func (s *schoolTestServer) recorded() []schoolRecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]schoolRecordedRequest(nil), s.requests...)
}

func (s *schoolTestServer) handleLanding(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if s.beforeLanding != nil {
		s.beforeLanding()
	}
	if s.blockLanding != nil {
		<-s.blockLanding
	}
	if s.landingRedirectTo != "" {
		http.Redirect(w, r, s.landingRedirectTo, http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: fakeSessionCookie, Value: fakeSessionValue, Path: "/"})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(s.landingStatus)
	if s.landingBody != nil {
		w.Write(s.landingBody)
		return
	}
	token := s.csrfToken
	if s.omitCSRF {
		fmt.Fprint(w, strings.Replace(fmt.Sprintf(schoolLandingHTML, token),
			`<meta name="csrf-token" content="`+token+`">`, "", 1))
		return
	}
	fmt.Fprintf(w, schoolLandingHTML, token)
}

func (s *schoolTestServer) handleSession(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if s.beforeSession != nil {
		s.beforeSession()
	}
	if s.sessionRedirectTo != "" {
		// 307 preserves the method and body, which is the case that re-sends the
		// multipart form to the redirect target.
		http.Redirect(w, r, s.sessionRedirectTo, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.sessionStatus)
	if s.sessionBody != nil {
		w.Write(s.sessionBody)
		return
	}
	payload := map[string]any{"status": "ok", "note": bodySecretSentinel}
	if !s.omitProfileInJSON {
		profileURL := s.profileURL
		if profileURL == "" {
			profileURL = s.server.URL + s.pathPrefix + fakeProfilePath
		}
		payload["profile"] = profileURL
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.t.Errorf("encode session response: %v", err)
	}
}

func (s *schoolTestServer) handleProfile(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if s.beforeProfile != nil {
		s.beforeProfile()
	}
	if s.profileRedirectTo != "" {
		http.Redirect(w, r, s.profileRedirectTo, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.WriteHeader(s.profileStatus)
	w.Write(s.profile)
}

func (s *schoolTestServer) handleProfileRedirect(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	http.Redirect(w, r, s.pathPrefix+fakeProfilePath, http.StatusFound)
}

func (s *schoolTestServer) input() SchoolInput {
	return SchoolInput{BaseURL: s.baseURL(), Network: fakeNetworkID, Location: fakeLocationID}
}

func requireStage(t *testing.T, err error, want Stage) *StageError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error at stage %q, got nil", want)
	}
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error is not a *StageError: %v", err)
	}
	if stageErr.Stage != want {
		t.Fatalf("error stage is %q, want %q: %v", stageErr.Stage, want, err)
	}
	return stageErr
}

// A School profile is a credential and an error page may itself carry session
// detail, so no response body may ever reach the error a caller sees. The network
// and location IDs are write-only config secrets and they travel in the landing
// page's query string, so a transport error that quotes the request URL would leak
// them too.
func requireNoSecretLeak(t *testing.T, err error) {
	t.Helper()
	message := err.Error()
	// "csrf-token" itself is not on this list: naming the meta tag we looked for is
	// a constant from our own source and is the diagnostic. The token's *value* is
	// the secret.
	for _, fragment := range []string{
		bodySecretSentinel, "<html", "<!DOCTYPE", fakeCSRFToken, fakeSessionValue,
		fakeNetworkID, fakeLocationID,
	} {
		if strings.Contains(message, fragment) {
			t.Fatalf("error leaked %q: %q", fragment, message)
		}
	}
}

func TestSchoolFetcherPerformsThreeStepHandshake(t *testing.T) {
	server := newSchoolTestServer(t)
	fetcher := NewSchoolFetcher(server.server.Client())

	profile, err := fetcher.FetchProfile(context.Background(), server.input())
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if !bytes.Equal(profile, server.profile) {
		t.Fatalf("FetchProfile returned %d bytes, want the %d downloaded bytes", len(profile), len(server.profile))
	}

	requests := server.recorded()
	if len(requests) != 3 {
		t.Fatalf("server saw %d requests, want 3: %+v", len(requests), requests)
	}

	landing := requests[0]
	if landing.method != http.MethodGet || landing.path != "/enroll/" {
		t.Errorf("step 1 is %s %s, want GET /enroll/", landing.method, landing.path)
	}
	if got := landing.query.Get("network"); got != fakeNetworkID {
		t.Errorf("step 1 network query is %q, want %q", got, fakeNetworkID)
	}
	if got := landing.query.Get("location"); got != fakeLocationID {
		t.Errorf("step 1 location query is %q, want %q", got, fakeLocationID)
	}

	session := requests[1]
	if session.method != http.MethodPost || session.path != "/enroll/startEnrollment.html" {
		t.Errorf("step 2 is %s %s, want POST /enroll/startEnrollment.html", session.method, session.path)
	}
	if !strings.HasPrefix(session.contentType, "multipart/form-data") {
		t.Errorf("step 2 Content-Type is %q, want multipart/form-data", session.contentType)
	}
	if got := session.header.Get("X-CSRF-TOKEN"); got != fakeCSRFToken {
		t.Errorf("step 2 X-CSRF-TOKEN is %q, want %q", got, fakeCSRFToken)
	}
	if got := session.header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("step 2 X-Requested-With is %q, want XMLHttpRequest", got)
	}
	wantForm := map[string]string{
		"userId":   "",
		"hash":     "",
		"network":  fakeNetworkID,
		"appleid":  "",
		"location": fakeLocationID,
	}
	for name, want := range wantForm {
		got, ok := session.form[name]
		if !ok {
			t.Errorf("step 2 multipart form is missing field %q (got %+v)", name, session.form)
			continue
		}
		if got != want {
			t.Errorf("step 2 form field %q is %q, want %q", name, got, want)
		}
	}

	download := requests[2]
	if download.method != http.MethodGet || download.path != fakeProfilePath {
		t.Errorf("step 3 is %s %s, want GET %s", download.method, download.path, fakeProfilePath)
	}
}

func TestSchoolFetcherCarriesTheSessionCookieThroughEveryStep(t *testing.T) {
	server := newSchoolTestServer(t)
	fetcher := NewSchoolFetcher(server.server.Client())

	if _, err := fetcher.FetchProfile(context.Background(), server.input()); err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}

	requests := server.recorded()
	if len(requests) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(requests))
	}
	if _, ok := requests[0].cookie(fakeSessionCookie); ok {
		t.Errorf("step 1 sent a session cookie before the server issued one")
	}
	for index, request := range requests[1:] {
		value, ok := request.cookie(fakeSessionCookie)
		if !ok {
			t.Errorf("step %d did not send the session cookie set by step 1", index+2)
			continue
		}
		if value != fakeSessionValue {
			t.Errorf("step %d sent cookie %q, want %q", index+2, value, fakeSessionValue)
		}
	}
}

// Each startEnrollment.html call mints a fresh MDM identity, so two fetches must
// not share a session: a reused cookie jar is how two VMs would end up sharing one
// identity and colliding in Jamf School.
func TestSchoolFetcherUsesAFreshCookieJarPerFetch(t *testing.T) {
	server := newSchoolTestServer(t)
	fetcher := NewSchoolFetcher(server.server.Client())

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := fetcher.FetchProfile(context.Background(), server.input()); err != nil {
			t.Fatalf("FetchProfile attempt %d: %v", attempt+1, err)
		}
	}

	requests := server.recorded()
	if len(requests) != 6 {
		t.Fatalf("server saw %d requests, want 6", len(requests))
	}
	if _, ok := requests[3].cookie(fakeSessionCookie); ok {
		t.Errorf("the second fetch's landing request reused the first fetch's session cookie")
	}
}

func TestSchoolFetcherRejectsIncompleteInput(t *testing.T) {
	server := newSchoolTestServer(t)
	fetcher := NewSchoolFetcher(server.server.Client())

	for _, testCase := range []struct {
		name  string
		input SchoolInput
	}{
		{"no base URL", SchoolInput{Network: fakeNetworkID, Location: fakeLocationID}},
		{"no network", SchoolInput{BaseURL: server.baseURL(), Location: fakeLocationID}},
		{"no location", SchoolInput{BaseURL: server.baseURL(), Network: fakeNetworkID}},
		{"unparseable base URL", SchoolInput{BaseURL: "not-a-url", Network: fakeNetworkID, Location: fakeLocationID}},
		{"non-http base URL", SchoolInput{BaseURL: "file:///etc/passwd", Network: fakeNetworkID, Location: fakeLocationID}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fetcher.FetchProfile(context.Background(), testCase.input)
			requireStage(t, err, StageConfiguration)
		})
	}
	if requests := server.recorded(); len(requests) != 0 {
		t.Errorf("incomplete input still reached the server: %+v", requests)
	}
}

func TestSchoolFetcherReportsTheStageThatFailed(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		configure func(*schoolTestServer)
		wantStage Stage
	}{
		{"landing page returns 500", func(s *schoolTestServer) { s.landingStatus = http.StatusInternalServerError }, StageLanding},
		{"landing page returns 404", func(s *schoolTestServer) { s.landingStatus = http.StatusNotFound }, StageLanding},
		{"landing page has no csrf meta tag", func(s *schoolTestServer) { s.omitCSRF = true }, StageLanding},
		{"landing page has an empty csrf token", func(s *schoolTestServer) { s.csrfToken = "" }, StageLanding},
		{"landing page is oversized", func(s *schoolTestServer) {
			s.landingBody = []byte("<html>" + strings.Repeat("x", maxSchoolProfileBytes) + bodySecretSentinel + "</html>")
		}, StageLanding},
		{"session returns 403", func(s *schoolTestServer) { s.sessionStatus = http.StatusForbidden }, StageSession},
		{"session body is not json", func(s *schoolTestServer) {
			s.sessionBody = []byte("<html>" + bodySecretSentinel + "</html>")
		}, StageSession},
		{"session json omits profile", func(s *schoolTestServer) { s.omitProfileInJSON = true }, StageSession},
		{"session json has an empty profile", func(s *schoolTestServer) {
			s.sessionBody = []byte(`{"profile":"","note":"` + bodySecretSentinel + `"}`)
		}, StageSession},
		{"session json profile is unparseable", func(s *schoolTestServer) {
			s.profileURL = "http://[::1" + bodySecretSentinel
		}, StageSession},
		{"session json profile has a non-http scheme", func(s *schoolTestServer) {
			s.profileURL = "file:///etc/passwd"
		}, StageSession},
		{"session body is oversized", func(s *schoolTestServer) {
			s.sessionBody = []byte(`{"note":"` + strings.Repeat("x", maxSchoolProfileBytes) + bodySecretSentinel + `"}`)
		}, StageSession},
		{"profile download returns 401", func(s *schoolTestServer) { s.profileStatus = http.StatusUnauthorized }, StageDownload},
		{"profile download returns a login page", func(s *schoolTestServer) {
			s.profile = []byte("<!DOCTYPE html><html>Sign in " + bodySecretSentinel + "</html>")
		}, StageDownload},
		{"profile download is empty", func(s *schoolTestServer) { s.profile = []byte{} }, StageDownload},
		{"profile download is oversized", func(s *schoolTestServer) {
			s.profile = append(syntheticSignedDataCMS(t), bytes.Repeat([]byte("x"), maxSchoolProfileBytes)...)
		}, StageDownload},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newSchoolTestServer(t)
			testCase.configure(server)
			fetcher := NewSchoolFetcher(server.server.Client())

			_, err := fetcher.FetchProfile(context.Background(), server.input())
			requireStage(t, err, testCase.wantStage)
			requireNoSecretLeak(t, err)
		})
	}
}

func TestSchoolFetcherTransportErrorDoesNotLeakTheRequestURL(t *testing.T) {
	server := newSchoolTestServer(t)
	input := server.input()
	fetcher := NewSchoolFetcher(server.server.Client())
	server.server.Close() // nothing is listening any more

	_, err := fetcher.FetchProfile(context.Background(), input)
	requireStage(t, err, StageLanding)
	requireNoSecretLeak(t, err)
}

func TestSchoolFetcherResolvesARelativeProfileURL(t *testing.T) {
	server := newSchoolTestServer(t)
	server.profileURL = fakeProfilePath
	fetcher := NewSchoolFetcher(server.server.Client())

	profile, err := fetcher.FetchProfile(context.Background(), server.input())
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if !bytes.Equal(profile, server.profile) {
		t.Fatalf("FetchProfile returned %d bytes, want %d", len(profile), len(server.profile))
	}
}

func TestSchoolFetcherStopsWhenTheContextIsAlreadyCancelled(t *testing.T) {
	server := newSchoolTestServer(t)
	fetcher := NewSchoolFetcher(server.server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetcher.FetchProfile(ctx, server.input())
	requireStage(t, err, StageLanding)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
	if requests := server.recorded(); len(requests) != 0 {
		t.Errorf("a cancelled context still reached the server: %+v", requests)
	}
}

func TestSchoolFetcherStopsWhenTheContextIsCancelledMidHandshake(t *testing.T) {
	server := newSchoolTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	server.beforeSession = cancel
	fetcher := NewSchoolFetcher(server.server.Client())

	_, err := fetcher.FetchProfile(ctx, server.input())
	requireStage(t, err, StageSession)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
	if requests := server.recorded(); len(requests) != 2 {
		t.Errorf("server saw %d requests, want 2 (the download must not run)", len(requests))
	}
}

// A base URL may carry a path prefix. This pins schoolPath, which is the only
// non-trivial URL construction in the handshake.
func TestSchoolFetcherHonoursABaseURLPathPrefix(t *testing.T) {
	server := newSchoolTestServerAt(t, "/jss")
	fetcher := NewSchoolFetcher(server.server.Client())

	profile, err := fetcher.FetchProfile(context.Background(), server.input())
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if !bytes.Equal(profile, server.profile) {
		t.Fatalf("FetchProfile returned %d bytes, want %d", len(profile), len(server.profile))
	}

	requests := server.recorded()
	if len(requests) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(requests))
	}
	wantPaths := []string{"/jss/enroll/", "/jss/enroll/startEnrollment.html", "/jss" + fakeProfilePath}
	for index, want := range wantPaths {
		if requests[index].path != want {
			t.Errorf("step %d path is %q, want %q", index+1, requests[index].path, want)
		}
	}
}

// The handshake must be bounded whatever client the caller supplies: the request
// runs on a task goroutine, and a server that accepts the connection then never
// writes would otherwise leave a task banner up forever. http.Client.Timeout is
// per-request, so it cannot be relied on for this even when it is set.
func TestSchoolFetcherBoundsTheWholeHandshakeRegardlessOfTheClient(t *testing.T) {
	server := newSchoolTestServer(t)
	release := make(chan struct{})
	server.blockLanding = release
	t.Cleanup(func() { close(release) })

	clientWithoutTimeout := &http.Client{Transport: server.server.Client().Transport}
	fetcher := &schoolFetcher{client: clientWithoutTimeout, timeout: 50 * time.Millisecond}

	_, err := fetcher.FetchProfile(context.Background(), server.input())
	requireStage(t, err, StageLanding)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error does not wrap context.DeadlineExceeded: %v", err)
	}
	requireNoSecretLeak(t, err)
}

// Go strips Cookie and Authorization on a cross-host redirect but keeps custom
// headers, the Referer, and — on a 307 — the request body. Following one would
// hand X-CSRF-TOKEN, the landing URL's network and location query parameters, and
// the multipart form to a third party. A tenant fronting Jamf School with SSO can
// produce exactly this redirect without anyone acting maliciously.
func TestSchoolFetcherRefusesACrossHostRedirect(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		configure func(*schoolTestServer, string)
		wantStage Stage
	}{
		{"on the landing page", func(s *schoolTestServer, target string) { s.landingRedirectTo = target }, StageLanding},
		{"on startEnrollment", func(s *schoolTestServer, target string) { s.sessionRedirectTo = target }, StageSession},
		// Note this redirects the download; it does not point profileURL at another
		// host. A cross-host profile URL in the startEnrollment JSON is deliberately
		// still allowed — see the ledger ruling on not pinning the host.
		{"on the profile download", func(s *schoolTestServer, target string) { s.profileRedirectTo = target }, StageDownload},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			elsewhere := newSchoolTestServer(t)
			server := newSchoolTestServer(t)
			testCase.configure(server, elsewhere.baseURL()+"/enroll/")
			fetcher := NewSchoolFetcher(server.server.Client())

			_, err := fetcher.FetchProfile(context.Background(), server.input())
			requireStage(t, err, testCase.wantStage)
			requireNoSecretLeak(t, err)
			if requests := elsewhere.recorded(); len(requests) != 0 {
				t.Fatalf("the redirect target was contacted with %d request(s): %+v", len(requests), requests)
			}
		})
	}
}

// Same-host redirects stay allowed: a server normalising a path, or bouncing http
// to https on its own hostname, is ordinary and leaks nothing.
func TestSchoolFetcherFollowsASameHostRedirect(t *testing.T) {
	server := newSchoolTestServer(t)
	server.profileURL = fakeRedirectedProfilePath
	fetcher := NewSchoolFetcher(server.server.Client())

	profile, err := fetcher.FetchProfile(context.Background(), server.input())
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if !bytes.Equal(profile, server.profile) {
		t.Fatalf("FetchProfile returned %d bytes, want %d", len(profile), len(server.profile))
	}
	if requests := server.recorded(); len(requests) != 4 {
		t.Fatalf("server saw %d requests, want 4 (landing, session, redirect, profile)", len(requests))
	}
}

func TestNewSchoolFetcherAlwaysSetsAHandshakeTimeout(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		client *http.Client
	}{
		{"no client supplied", nil},
		{"caller supplied a client with no timeout of its own", &http.Client{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fetcher, ok := NewSchoolFetcher(testCase.client).(*schoolFetcher)
			if !ok {
				t.Fatalf("NewSchoolFetcher did not return a *schoolFetcher")
			}
			if fetcher.client == nil {
				t.Fatalf("NewSchoolFetcher left the client nil")
			}
			if fetcher.timeout <= 0 {
				t.Fatalf("fetcher has no handshake timeout")
			}
		})
	}
}

// The fetcher must not install a jar on the caller's client: the caller owns it,
// and a shared jar would outlive one fetch.
func TestSchoolFetcherDoesNotMutateTheProvidedClient(t *testing.T) {
	server := newSchoolTestServer(t)
	client := server.server.Client()
	fetcher := NewSchoolFetcher(client)

	if _, err := fetcher.FetchProfile(context.Background(), server.input()); err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if client.Jar != nil {
		t.Fatalf("FetchProfile installed a cookie jar on the caller's client")
	}
}
