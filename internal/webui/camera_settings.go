package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxOSDTextLength = 64

type cameraSettings struct {
	OSDEnabled  bool   `json:"osd_enabled"`
	OSDText     string `json:"osd_text"`
	OSDFontSize int    `json:"osd_font_size"`
}

func validateCameraSettings(settings cameraSettings) error {
	settings.OSDText = strings.TrimSpace(settings.OSDText)
	if settings.OSDText == "" {
		return errors.New("OSD text must not be empty")
	}
	if !utf8.ValidString(settings.OSDText) || utf8.RuneCountInString(settings.OSDText) > maxOSDTextLength {
		return fmt.Errorf("OSD text must be valid UTF-8 and no longer than %d characters", maxOSDTextLength)
	}
	for _, character := range settings.OSDText {
		if unicode.IsControl(character) {
			return errors.New("OSD text must not contain control characters")
		}
	}
	if settings.OSDFontSize < 8 || settings.OSDFontSize > 96 {
		return errors.New("OSD font size must be between 8 and 96")
	}
	return nil
}

func loadCameraSettings(path string, defaults cameraSettings) (cameraSettings, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return cameraSettings{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8<<10))
	decoder.DisallowUnknownFields()
	var settings cameraSettings
	if err := decoder.Decode(&settings); err != nil {
		return cameraSettings{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return cameraSettings{}, errors.New("camera settings contain trailing data")
	}
	if err := validateCameraSettings(settings); err != nil {
		return cameraSettings{}, err
	}
	return settings, nil
}

func saveCameraSettings(path string, settings cameraSettings) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".camera-settings-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (s *Server) currentCameraSettings() cameraSettings {
	s.cameraSettingsMu.RLock()
	defer s.cameraSettingsMu.RUnlock()
	return s.cameraSettings
}

func (s *Server) getCameraSettings(w http.ResponseWriter) {
	settings := s.currentCameraSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":       s.config.CameraBackend,
		"osd_supported": s.config.CameraBackend == "gstreamer",
		"osd_enabled":   settings.OSDEnabled,
		"osd_text":      settings.OSDText,
		"osd_font_size": settings.OSDFontSize,
		"osd_position":  "top-right",
	})
}

func (s *Server) putCameraSettings(w http.ResponseWriter, r *http.Request) {
	if s.cameraIsRunning() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stop live playback before changing camera settings"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var settings cameraSettings
	if err := decoder.Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid camera settings"})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid camera settings"})
		return
	}
	if err := validateCameraSettings(settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings.OSDText = strings.TrimSpace(settings.OSDText)
	if err := saveCameraSettings(s.config.CameraSettingsPath, settings); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot save camera settings"})
		return
	}
	s.cameraSettingsMu.Lock()
	s.cameraSettings = settings
	s.cameraSettingsMu.Unlock()
	s.getCameraSettings(w)
}
