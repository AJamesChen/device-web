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

func (s *Server) cameraCommandSpec(frameCount int) (string, []string) {
	if s.config.CameraBackend == "gstreamer" {
		args := []string{"-q", "v4l2src", "device=" + s.config.CameraDevice, "io-mode=mmap"}
		if frameCount > 0 {
			args = append(args, "num-buffers="+strconv.Itoa(frameCount))
		}
		args = append(args,
			"!", "image/jpeg,width="+strconv.Itoa(s.config.CameraWidth)+
				",height="+strconv.Itoa(s.config.CameraHeight)+
				",framerate="+strconv.Itoa(s.config.CameraFPS)+"/1",
		)
		settings := s.currentCameraSettings()
		if settings.OSDEnabled {
			args = append(args,
				"!", "jpegdec",
				"!", "videoconvert",
				"!", "textoverlay",
				"text="+settings.OSDText,
				"halignment=right", "valignment=top",
				"font-desc=Sans "+strconv.Itoa(settings.OSDFontSize),
				"xpad=12", "ypad=12",
				"draw-shadow=true", "draw-outline=true",
				"!", "jpegenc", "quality=90",
			)
		}
		args = append(args, "!", "fdsink", "fd=1", "sync=false")
		return "gst-launch-1.0", args
	}

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
	return "v4l2-ctl", args
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

	program, args := s.cameraCommandSpec(0)
	command := s.cameraCommand(ctx, program, args...)
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
	running := s.cameraIsRunning()
	state := "stopped"
	if running {
		state = "streaming"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":   state,
		"backend": s.config.CameraBackend,
		"device":  s.config.CameraDevice,
		"width":   s.config.CameraWidth,
		"height":  s.config.CameraHeight,
		"fps":     s.config.CameraFPS,
	})
}

func (s *Server) cameraIsRunning() bool {
	s.cameraStateMu.Lock()
	defer s.cameraStateMu.Unlock()
	return s.cameraCancel != nil
}

func (s *Server) snapshotCamera(w http.ResponseWriter, r *http.Request) {
	ctx, finish, ok := s.beginCamera(r.Context())
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stop live playback before taking a snapshot"})
		return
	}
	defer finish()

	program, args := s.cameraCommandSpec(1)
	command := s.cameraCommand(ctx, program, args...)
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

	// Parse length-delimited JPEG header segments so SOI/EOI bytes inside an
	// EXIF thumbnail are not mistaken for boundaries of the outer image.
	for {
		markerPrefix, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if markerPrefix != 0xff {
			return nil, errors.New("invalid JPEG marker prefix")
		}
		if err := appendJPEGBytes(&frame, markerPrefix); err != nil {
			return nil, err
		}
		marker, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		for marker == 0xff {
			if err := appendJPEGBytes(&frame, marker); err != nil {
				return nil, err
			}
			marker, err = reader.ReadByte()
			if err != nil {
				return nil, err
			}
		}
		if err := appendJPEGBytes(&frame, marker); err != nil {
			return nil, err
		}
		if marker == 0xd9 {
			return frame, nil
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd8 {
			continue
		}

		lengthBytes := make([]byte, 2)
		if _, err := io.ReadFull(reader, lengthBytes); err != nil {
			return nil, err
		}
		if err := appendJPEGBytes(&frame, lengthBytes...); err != nil {
			return nil, err
		}
		segmentLength := int(lengthBytes[0])<<8 | int(lengthBytes[1])
		if segmentLength < 2 {
			return nil, errors.New("invalid JPEG segment length")
		}
		payload := make([]byte, segmentLength-2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		if err := appendJPEGBytes(&frame, payload...); err != nil {
			return nil, err
		}
		if marker == 0xda {
			break
		}
	}

	// Entropy-coded scan data uses FF00 byte stuffing; only an unescaped FFD9
	// terminates this baseline JPEG frame. Restart markers are also valid.
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if err := appendJPEGBytes(&frame, value); err != nil {
			return nil, err
		}
		if value != 0xff {
			continue
		}
		marker, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if err := appendJPEGBytes(&frame, marker); err != nil {
			return nil, err
		}
		switch {
		case marker == 0x00 || marker >= 0xd0 && marker <= 0xd7:
			continue
		case marker == 0xd9:
			return frame, nil
		default:
			return nil, errors.New("unsupported marker in JPEG scan data")
		}
	}
}

func appendJPEGBytes(frame *[]byte, values ...byte) error {
	if len(*frame)+len(values) > maxJPEGSize {
		return errors.New("JPEG frame exceeds size limit")
	}
	*frame = append(*frame, values...)
	return nil
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
