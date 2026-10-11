package webui

import (
	"bufio"
	"bytes"
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
	if status["state"] != "stopped" || status["backend"] != "v4l2" || status["device"] != "/dev/video0" {
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
	want := string(testJPEG())
	if snapshot.StatusCode != http.StatusOK || snapshot.Header.Get("Content-Type") != "image/jpeg" || string(picture) != want {
		t.Fatalf("snapshot response = %d %q %x", snapshot.StatusCode, snapshot.Header.Get("Content-Type"), picture)
	}
}

func TestGStreamerCameraCommand(t *testing.T) {
	app, err := New(Config{
		Username: "admin", Password: "secret", OTAAPIURL: "http://127.0.0.1:8080",
		OTAAPIToken: "token", MaxUpload: 1024, CameraBackend: "gstreamer",
		CameraDevice: "/dev/video7", CameraWidth: 1280, CameraHeight: 720, CameraFPS: 30,
		CameraOSDEnabled: true, CameraOSDText: "FoxTV", CameraOSDFontSize: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	program, args := app.cameraCommandSpec(1)
	joined := strings.Join(args, " ")
	if program != "gst-launch-1.0" {
		t.Fatalf("program = %q, want gst-launch-1.0", program)
	}
	for _, expected := range []string{
		"v4l2src device=/dev/video7 io-mode=mmap num-buffers=1",
		"image/jpeg,width=1280,height=720,framerate=30/1",
		"jpegdec ! videoconvert ! textoverlay text=FoxTV halignment=right valignment=top font-desc=Sans 20",
		"jpegenc quality=90",
		"fdsink fd=1 sync=false",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("GStreamer arguments %q do not contain %q", joined, expected)
		}
	}
	app.cameraSettings.OSDEnabled = false
	_, args = app.cameraCommandSpec(0)
	if strings.Contains(strings.Join(args, " "), "textoverlay") {
		t.Fatal("disabled OSD unexpectedly added textoverlay to GStreamer pipeline")
	}
}

func TestCameraSettingsRESTAPI(t *testing.T) {
	frontend, _, app := newTestServers(t)
	client := clientWithJar(t)
	csrf := login(t, client, frontend.URL)
	body := `{"osd_enabled":true,"osd_text":"FoxTV","osd_font_size":20}`

	request, _ := http.NewRequest(http.MethodPut, frontend.URL+"/api/camera/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	request, _ = http.NewRequest(http.MethodPut, frontend.URL+"/api/camera/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var settings map[string]any
	if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || settings["osd_enabled"] != true || settings["osd_text"] != "FoxTV" || settings["osd_font_size"] != float64(20) {
		t.Fatalf("unexpected camera settings response: status=%d body=%#v", response.StatusCode, settings)
	}
	if got := app.currentCameraSettings(); !got.OSDEnabled || got.OSDText != "FoxTV" || got.OSDFontSize != 20 {
		t.Fatalf("camera settings not applied: %#v", got)
	}
}

func TestCameraSettingsPersistence(t *testing.T) {
	path := t.TempDir() + "/camera-settings.json"
	want := cameraSettings{OSDEnabled: true, OSDText: "MaxTV", OSDFontSize: 20}
	if err := saveCameraSettings(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCameraSettings(path, cameraSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("loaded settings = %#v, want %#v", got, want)
	}
}

func TestRejectsUnknownCameraBackend(t *testing.T) {
	_, err := New(Config{
		Username: "admin", Password: "secret", OTAAPIURL: "http://127.0.0.1:8080",
		OTAAPIToken: "token", MaxUpload: 1024, CameraBackend: "unknown",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported camera backend") {
		t.Fatalf("New error = %v, want unsupported camera backend", err)
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
	_, _ = os.Stdout.Write(testJPEG())
	if mode == "stream" {
		time.Sleep(time.Hour)
	}
}

func TestReadJPEGSkipsEmbeddedEXIFThumbnail(t *testing.T) {
	jpeg := []byte{
		0xff, 0xd8,
		0xff, 0xe1, 0x00, 0x08, 0xff, 0xd8, 0xaa, 0xff, 0xd9, 0xbb,
		0xff, 0xda, 0x00, 0x02,
		0x11, 0xff, 0x00, 0x22, 0xff, 0xd9,
	}
	got, err := readJPEG(bufio.NewReader(bytes.NewReader(jpeg)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jpeg) {
		t.Fatalf("readJPEG returned %x, want %x", got, jpeg)
	}
}

func testJPEG() []byte {
	return []byte{0xff, 0xd8, 0xff, 0xda, 0x00, 0x02, 't', 'e', 's', 't', 0xff, 0xd9}
}
