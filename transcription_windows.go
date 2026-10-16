package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type speechSegment struct {
	Start, End float64
	Text       string
	NoSpeech   float64 `json:"no_speech_prob"`
}

type speechRecognizer func(context.Context, []byte) ([]speechSegment, error)

func defaultSpeechModel() string {
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "stt", "ggml-large-v3-turbo-q5_0.bin")
}

// One model per source, loaded lazily. The random URL and loopback binding keep the
// upstream server's model-loading/upload endpoints out of the session network.
func startWhisper(ctx context.Context, model string, cpu bool) (speechRecognizer, func(), error) {
	exe, _ := os.Executable()
	server := filepath.Join(filepath.Dir(exe), "stt", "whisper-server.exe")
	if _, err := os.Stat(server); err != nil {
		server = filepath.Join(filepath.Dir(model), "whisper-server.exe")
	}
	if _, err := os.Stat(model); err != nil {
		return nil, nil, fmt.Errorf("модель не найдена: %s", model)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	path := "/" + sessionKey() + sessionKey()
	base := "http://127.0.0.1:" + strconv.Itoa(port) + path
	child, cancel := context.WithCancel(ctx)
	args := []string{"-m", model, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--request-path", path, "-l", "ru", "-t", "4", "-nlp", "-bs", "1", "-bo", "1"}
	if cpu {
		args = append(args, "-ng")
	}
	cmd := exec.CommandContext(child, server, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	log, err := os.CreateTemp(filepath.Dir(server), "whisper-*.log")
	if err != nil {
		cancel()
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		cancel()
		log.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); log.Close(); close(done) }()
	closeServer := func() { cancel(); <-done }
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			closeServer()
			return nil, nil, ctx.Err()
		case <-done:
			cancel()
			return nil, nil, fmt.Errorf("Whisper завершился при загрузке; журнал: %s", log.Name())
		case <-timeout.C:
			closeServer()
			return nil, nil, fmt.Errorf("Whisper не загрузился за 2 минуты; журнал: %s", log.Name())
		case <-ticker.C:
			req, _ := http.NewRequestWithContext(ctx, "GET", base+"/health", nil)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return whisperRecognize(base), closeServer, nil
			}
		}
	}
}

func whisperRecognize(base string) speechRecognizer {
	client := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	return func(ctx context.Context, pcm []byte) ([]speechSegment, error) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", "speech.wav")
		header, _ := wavHeader(int64(len(pcm)/2), 1)
		binary.LittleEndian.PutUint32(header[24:], 16000)
		binary.LittleEndian.PutUint32(header[28:], 32000)
		part.Write(header)
		part.Write(pcm)
		for k, v := range map[string]string{"response_format": "verbose_json", "language": "ru", "translate": "false", "temperature": "0", "temperature_inc": "0", "no_context": "true"} {
			_ = form.WriteField(k, v)
		}
		form.Close()
		req, err := http.NewRequestWithContext(ctx, "POST", base+"/inference", &body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("Whisper: HTTP %d", resp.StatusCode)
		}
		var result struct{ Segments []speechSegment }
		err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result)
		return result.Segments, err
	}
}
