# Device Web Console

A small Go web service for managing the Raspberry Pi 4B Yocto image. It embeds
its browser assets in one native executable and proxies the loopback-only
`jamesc-ota-api`, so the OTA bearer token is never exposed to the browser.

## Features

- Login and server-side sessions
- Responsive system-information dashboard
- Click-to-select or drag-and-drop signed RAUC bundle upload with browser progress
- Live RAUC installation status
- Authoritative success/failure recovery if the upload response is interrupted
- Confirmed device reboot with automatic reconnect detection
- Camera tab with live MJPEG playback, stop control, and JPEG snapshots
- Authenticated REST camera API for browser and automation clients
- CSRF protection, strict cookies, security headers, and upload limits
- Standard-library-only Go build with selectable `v4l2-ctl` and GStreamer camera backends

## Run locally

Go 1.22 or later is required.

```sh
go test ./...
go run ./cmd/device-web
```

Build a static ARM64 binary for the Raspberry Pi:

```sh
make build-arm64
```

Open <http://localhost:8081> and sign in with the development defaults:

- Username: `admin`
- Password: `jamesc`

The dashboard expects `jamesc-ota-api` at `http://127.0.0.1:8080`.

## Install firmware

Open the firmware update panel and either click the drop area or drag one
signed `.raucb` bundle onto it. The browser rejects multiple files, empty
files, filenames without the `.raucb` suffix, and bundles larger than the
default 384 MiB limit before upload. Confirm the selected filename and size,
then choose **Upload and install**.

Keep the device powered while the page reports `uploading` or `installing`.
Reboot only after it reports `succeeded`; RAUC verifies the bundle signature
and compatibility and writes the inactive A/B slot.

The dashboard polls the OTA API through installation and displays the final
success or failure state. If the browser loses the upload response, it queries
the OTA status before reporting an error, avoiding a false failure when RAUC
has already accepted or installed the bundle.

Use **Reboot** in the header to restart the device. The action requires an
authenticated session, CSRF token, and confirmation. Reboot is rejected while
a firmware upload or RAUC installation is active. The page waits for the
device to go offline and redirects to login after it returns.

## Camera and REST API

Open the **Camera** tab and choose **Play video** to view the MJPEG stream,
**Stop playback** to release the V4L2 device, or **Snap picture** to capture and
download a JPEG. Only one stream or snapshot operation can own the camera at a
time. Taking a picture from the page stops live playback first.

The same functions are available through the authenticated REST API:

| Method | Endpoint | Result |
| --- | --- | --- |
| `GET` | `/api/camera` | Camera state and configured format as JSON |
| `GET` | `/api/camera/stream` | Live `multipart/x-mixed-replace` MJPEG response |
| `POST` | `/api/camera/stop` | Stops capture and returns `{"state":"stopped"}` |
| `POST` | `/api/camera/snapshot` | Captures and returns one `image/jpeg` |

All endpoints require the login session cookie. The two `POST` calls also
require the session's `csrf_token` from `GET /api/session` in the
`X-CSRF-Token` header. For example, after logging in and saving cookies:

```sh
curl -c cookies.txt -d 'username=admin' -d 'password=YOUR_PASSWORD' \
  http://DEVICE:8081/login
CSRF=$(curl -sS -b cookies.txt http://DEVICE:8081/api/session | \
  sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
curl -b cookies.txt http://DEVICE:8081/api/camera/stream -o live.mjpeg
curl -b cookies.txt -H "X-CSRF-Token: $CSRF" -X POST \
  http://DEVICE:8081/api/camera/snapshot -o snapshot.jpg
curl -b cookies.txt -H "X-CSRF-Token: $CSRF" -X POST \
  http://DEVICE:8081/api/camera/stop
```

The service account must be able to open the configured V4L2 node. The
supplied systemd unit adds its dynamic user to the `video` group.

### GStreamer backend

Set `DEVICE_WEB_CAMERA_BACKEND=gstreamer` to capture through GStreamer instead
of invoking `v4l2-ctl`. The service runs this pipeline for live playback:

```sh
gst-launch-1.0 -q v4l2src device=/dev/video0 io-mode=mmap \
  ! 'image/jpeg,width=640,height=480,framerate=15/1' \
  ! fdsink fd=1 sync=false
```

Snapshots use the same pipeline with `num-buffers=1`. The camera must provide
JPEG/MJPEG at the configured resolution and frame rate. Install the GStreamer
core tools and the `video4linux2` plugin; the Yocto recipe declares both when
building the project image. The stream parser follows JPEG segment lengths so
embedded EXIF thumbnails do not truncate playback frames. Starting playback
also stops any stale capture before opening a new stream. `GET /api/camera`
reports the active backend.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `DEVICE_WEB_LISTEN` | `:8081` | Web listen address |
| `DEVICE_WEB_USERNAME` | `admin` | Login username |
| `DEVICE_WEB_PASSWORD` | `jamesc` | Login password |
| `DEVICE_WEB_OTA_API_URL` | `http://127.0.0.1:8080` | Internal OTA API URL |
| `DEVICE_WEB_OTA_TOKEN` | `dca6327c4e1e` | Internal OTA API bearer token |
| `DEVICE_WEB_MAX_UPLOAD` | `402653184` | Maximum firmware bytes |
| `DEVICE_WEB_SECURE_COOKIES` | `false` | Require HTTPS for session cookies |
| `DEVICE_WEB_CAMERA_DEVICE` | `/dev/video0` | V4L2 capture device |
| `DEVICE_WEB_CAMERA_WIDTH` | `640` | Capture width in pixels |
| `DEVICE_WEB_CAMERA_HEIGHT` | `480` | Capture height in pixels |
| `DEVICE_WEB_CAMERA_FPS` | `15` | Requested stream frame rate |
| `DEVICE_WEB_CAMERA_BACKEND` | `v4l2` | Capture backend: `v4l2` or `gstreamer` |

The supplied files in `deploy/` show the intended systemd configuration.

## Production security

The default password and MAC-derived OTA token are development credentials.
Change both before deployment. Terminate TLS in front of the service and set
`DEVICE_WEB_SECURE_COOKIES=true`. The application intentionally keeps the OTA
API on loopback and does not return its bearer token to browser clients.

Sessions are stored in memory and expire after 12 hours, so restarting the
service signs out all users. Firmware is streamed through to the OTA service
and is not buffered by this application.
