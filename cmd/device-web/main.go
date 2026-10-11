package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AJamesChen/device-web/internal/webui"
)

func main() {
	cfg := webui.Config{
		Username:           env("DEVICE_WEB_USERNAME", "admin"),
		Password:           env("DEVICE_WEB_PASSWORD", "jamesc"),
		OTAAPIURL:          env("DEVICE_WEB_OTA_API_URL", "http://127.0.0.1:8080"),
		OTAAPIToken:        env("DEVICE_WEB_OTA_TOKEN", "dca6327c4e1e"),
		SecureCookies:      envBool("DEVICE_WEB_SECURE_COOKIES", false),
		MaxUpload:          envInt64("DEVICE_WEB_MAX_UPLOAD", 384*1024*1024),
		CameraDevice:       env("DEVICE_WEB_CAMERA_DEVICE", "/dev/video0"),
		CameraWidth:        envInt("DEVICE_WEB_CAMERA_WIDTH", 640),
		CameraHeight:       envInt("DEVICE_WEB_CAMERA_HEIGHT", 480),
		CameraFPS:          envInt("DEVICE_WEB_CAMERA_FPS", 15),
		CameraBackend:      env("DEVICE_WEB_CAMERA_BACKEND", "v4l2"),
		CameraOSDEnabled:   envBool("DEVICE_WEB_CAMERA_OSD_ENABLED", false),
		CameraOSDText:      env("DEVICE_WEB_CAMERA_OSD_TEXT", "MaxTV"),
		CameraOSDFontSize:  envInt("DEVICE_WEB_CAMERA_OSD_FONT_SIZE", 20),
		CameraSettingsPath: env("DEVICE_WEB_CAMERA_SETTINGS_PATH", "/var/lib/device-web/camera-settings.json"),
	}

	app, err := webui.New(cfg)
	if err != nil {
		slog.Error("configure web service", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              env("DEVICE_WEB_LISTEN", ":8081"),
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("shutdown web service", "error", err)
		}
	}()

	slog.Info("device web service listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		slog.Warn("invalid boolean environment value", "name", name, "value", value)
		return fallback
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		slog.Warn("invalid positive integer environment value", "name", name, "value", value)
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		slog.Warn("invalid positive integer environment value", "name", name, "value", value)
		return fallback
	}
	return parsed
}
