package webui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cameraBoundary = "device-camera-frame"
	maxJPEGSize    = 16 << 20
)

func (s *Server) cameraArgs(frameCount int) []string {
	args := []string{
		"--device=" + s.config.CameraDevice,
		"--set-fmt-video=width=" + strconv.Itoa(s.config.CameraWidth) +
			",height=" + strconv.Itoa(s.config.CameraHeight) + ",pixelformat=MJPG",
		"--set-parm=" + strconv.Itoa(s.config.CameraFPS),
		"--stream-mmap=4",
		"--stream-to=-",
	}
	if frameCount > 0 {
		args = append(args, "--stream-count="+strconv.Itoa(frameCount))
	}
	return args
}

func (s *Server) beginCamera(requestContext context.Context) (context.Context, func(), bool) {
	if !s.cameraRun.TryLock() {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(requestContext)
	done := make(chan struct{})
	s.cameraStateMu.Lock()
	s.cameraCancel = cancel
	s.cameraDone = done
	s.cameraStateMu.Unlock()

	finish := func() {
		cancel()
		s.cameraStateMu.Lock()
		if s.cameraDone == done {
			s.cameraCancel = nil
			s.cameraDone = nil
		}
		close(done)
		s.cameraStateMu.Unlock()
		s.cameraRun.Unlock()
	}
	return ctx, finish, true
}

func (s *Server) streamCamera(w http.ResponseWriter, r *http.Request) {
	ctx, finish, ok := s.beginCamera(r.Context())
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "camera is already in use"})
		return
	}
	defer finish()

	command := s.cameraCommand(ctx, "v4l2-ctl", s.cameraArgs(0)...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot open camera output"})
		return
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cannot start camera capture"})
		return
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	reader := bufio.NewReader(stdout)
	frame, err := readJPEG(reader)
	if err != nil {
		_ = command.Wait()
		waited = true
		message := "camera did not return a JPEG frame"
		if detail := stringsForLog(stderr.String()); detail != "" {
			message += ": " + detail
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
		return
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+cameraBoundary)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for {
		if err := writeMJPEGFrame(w, frame); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		frame, err = readJPEG(reader)
		if err != nil {
			_ = command.Wait()
			waited = true
			return
		}
	}
}

func (s *Server) stopCamera(w http.ResponseWriter, r *http.Request) {
	s.cameraStateMu.Lock()
	cancel := s.cameraCancel
	done := s.cameraDone
	s.cameraStateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "camera did not stop in time"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "stopped"})
}

func (s *Server) cameraStatus(w http.ResponseWriter) {
	s.cameraStateMu.Lock()
	running := s.cameraCancel != nil
	s.cameraStateMu.Unlock()
	state := "stopped"
	if running {
		state = "streaming"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":  state,
		"device": s.config.CameraDevice,
		"width":  s.config.CameraWidth,
		"height": s.config.CameraHeight,
		"fps":    s.config.CameraFPS,
	})
}

func (s *Server) snapshotCamera(w http.ResponseWriter, r *http.Request) {
	ctx, finish, ok := s.beginCamera(r.Context())
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stop live playback before taking a snapshot"})
		return
	}
	defer finish()

	command := s.cameraCommand(ctx, "v4l2-ctl", s.cameraArgs(1)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := "camera snapshot failed"
		if detail := stringsForLog(stderr.String()); detail != "" {
			message += ": " + detail
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
		return
	}
	frame, err := readJPEG(bufio.NewReader(bytes.NewReader(stdout.Bytes())))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "camera returned an invalid snapshot"})
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", `inline; filename="camera-snapshot.jpg"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
}

func readJPEG(reader *bufio.Reader) ([]byte, error) {
	frame := make([]byte, 0, 256<<10)
	previous := byte(0)
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if previous == 0xff && value == 0xd8 {
			frame = append(frame, previous, value)
			break
		}
		previous = value
	}
	previous = 0
	for len(frame) < maxJPEGSize {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		frame = append(frame, value)
		if previous == 0xff && value == 0xd9 {
			return frame, nil
		}
		previous = value
	}
	return nil, errors.New("JPEG frame exceeds size limit")
}

func writeMJPEGFrame(writer io.Writer, frame []byte) error {
	if _, err := fmt.Fprintf(writer, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", cameraBoundary, len(frame)); err != nil {
		return err
	}
	if _, err := writer.Write(frame); err != nil {
		return err
	}
	_, err := io.WriteString(writer, "\r\n")
	return err
}

func stringsForLog(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}
