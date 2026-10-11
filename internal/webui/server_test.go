package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testToken = "dca6327c4e1e"

func newTestServers(t *testing.T) (*httptest.Server, *httptest.Server, *Server) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/system":
			writeJSON(w, http.StatusOK, map[string]string{"hostname": "test-pi"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/update":
			writeJSON(w, http.StatusOK, map[string]string{"state": "idle"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/update":
			body, _ := io.ReadAll(r.Body)
			if string(body) != "signed bundle" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"state": "installing"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/reboot":
			writeJSON(w, http.StatusAccepted, map[string]string{"state": "rebooting"})
		default:
			http.NotFound(w, r)
		}
	}))
	app, err := New(Config{
		Username: "admin", Password: "jamesc", OTAAPIURL: backend.URL,
		OTAAPIToken: testToken, MaxUpload: 1024,
	})
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	frontend := httptest.NewServer(app)
	t.Cleanup(frontend.Close)
	t.Cleanup(backend.Close)
	return frontend, backend, app
}

func clientWithJar(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func login(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	response, err := client.PostForm(baseURL+"/login", url.Values{"username": {"admin"}, "password": {"jamesc"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	response, err = client.Get(baseURL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var sessionData map[string]string
	if err := json.NewDecoder(response.Body).Decode(&sessionData); err != nil {
		t.Fatal(err)
	}
	if sessionData["username"] != "admin" || sessionData["csrf_token"] == "" {
		t.Fatalf("unexpected session response: %#v", sessionData)
	}
	return sessionData["csrf_token"]
}

func TestLoginAndAuthenticatedProxy(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)

	response, err := client.Get(frontend.URL + "/api/system")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	login(t, client, frontend.URL)
	response, err = client.Get(frontend.URL + "/api/system")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("system status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var system map[string]string
	if err := json.NewDecoder(response.Body).Decode(&system); err != nil {
		t.Fatal(err)
	}
	if system["hostname"] != "test-pi" {
		t.Fatalf("hostname = %q, want test-pi", system["hostname"])
	}
}

func TestInvalidLogin(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)
	response, err := client.PostForm(frontend.URL+"/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/login?error=1" {
		t.Fatalf("invalid login response = %d %q", response.StatusCode, response.Header.Get("Location"))
	}
}

func TestFirmwareUploadRequiresCSRFAndStreamsBundle(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)
	csrf := login(t, client, frontend.URL)

	request, _ := http.NewRequest(http.MethodPost, frontend.URL+"/api/firmware", strings.NewReader("signed bundle"))
	request.ContentLength = int64(len("signed bundle"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	request, _ = http.NewRequest(http.MethodPost, frontend.URL+"/api/firmware", strings.NewReader("signed bundle"))
	request.ContentLength = int64(len("signed bundle"))
	request.Header.Set("X-CSRF-Token", csrf)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upload status = %d, want %d: %s", response.StatusCode, http.StatusAccepted, body)
	}
}

func TestRejectsOversizedFirmware(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)
	csrf := login(t, client, frontend.URL)
	request, _ := http.NewRequest(http.MethodPost, frontend.URL+"/api/firmware", strings.NewReader(strings.Repeat("x", 1025)))
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestRebootRequiresCSRFAndProxiesRequest(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)
	csrf := login(t, client, frontend.URL)

	request, _ := http.NewRequest(http.MethodPost, frontend.URL+"/api/reboot", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	request, _ = http.NewRequest(http.MethodPost, frontend.URL+"/api/reboot", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("reboot status = %d, want %d: %s", response.StatusCode, http.StatusAccepted, body)
	}
}

func TestCameraRESTAPI(t *testing.T) {
	frontend, _, app := newTestServers(t)
	app.cameraCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		mode := "stream"
		for _, arg := range args {
			if arg == "--stream-count=1" {
				mode = "snapshot"
			}
		}
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCameraCommandHelper")
		command.Env = append(os.Environ(), "DEVICE_WEB_CAMERA_TEST="+mode)
		return command
	}
	client := clientWithJar(t)
	csrf := login(t, client, frontend.URL)

	response, err := client.Get(frontend.URL + "/api/camera")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if status["state"] != "stopped" || status["device"] != "/dev/video0" {
		t.Fatalf("unexpected camera status: %#v", status)
	}

	response, err = client.Get(frontend.URL + "/api/camera/stream")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "multipart/x-mixed-replace") {
		t.Fatalf("stream response = %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	defer response.Body.Close()

	request, _ := http.NewRequest(http.MethodPost, frontend.URL+"/api/camera/stop", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	stopResponse, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	stopResponse.Body.Close()
	if stopResponse.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want %d", stopResponse.StatusCode, http.StatusOK)
	}

	request, _ = http.NewRequest(http.MethodPost, frontend.URL+"/api/camera/snapshot", nil)
	request.Header.Set("X-CSRF-Token", csrf)
	snapshot, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Body.Close()
	picture, err := io.ReadAll(snapshot.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := string([]byte{0xff, 0xd8, 't', 'e', 's', 't', 0xff, 0xd9})
	if snapshot.StatusCode != http.StatusOK || snapshot.Header.Get("Content-Type") != "image/jpeg" || string(picture) != want {
		t.Fatalf("snapshot response = %d %q %x", snapshot.StatusCode, snapshot.Header.Get("Content-Type"), picture)
	}
}

func TestCameraMutationRequiresCSRF(t *testing.T) {
	frontend, _, _ := newTestServers(t)
	client := clientWithJar(t)
	login(t, client, frontend.URL)
	for _, path := range []string{"/api/camera/stop", "/api/camera/snapshot"} {
		request, _ := http.NewRequest(http.MethodPost, frontend.URL+path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want %d", path, response.StatusCode, http.StatusForbidden)
		}
	}
}

func TestCameraCommandHelper(t *testing.T) {
	mode := os.Getenv("DEVICE_WEB_CAMERA_TEST")
	if mode == "" {
		return
	}
	_, _ = os.Stdout.Write([]byte{0xff, 0xd8, 't', 'e', 's', 't', 0xff, 0xd9})
	if mode == "stream" {
		time.Sleep(time.Hour)
	}
}
