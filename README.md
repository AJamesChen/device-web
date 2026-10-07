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
- CSRF protection, strict cookies, security headers, and upload limits
- Standard-library-only Go build with no runtime dependencies

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

The supplied files in `deploy/` show the intended systemd configuration.

## Production security

The default password and MAC-derived OTA token are development credentials.
Change both before deployment. Terminate TLS in front of the service and set
`DEVICE_WEB_SECURE_COOKIES=true`. The application intentionally keeps the OTA
API on loopback and does not return its bearer token to browser clients.

Sessions are stored in memory and expire after 12 hours, so restarting the
service signs out all users. Firmware is streamed through to the OTA service
and is not buffered by this application.
